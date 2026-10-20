// Package gorelay talks to the OpenCode Go relay over its Responses API
// (POST {base}/v1/responses). It is the bot's only model backend: there is
// no fallback model, so every failure path resolves to an in-character
// canned reply instead of a downgrade.
package gorelay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// Busy is the rate-limit reply: an in-character "hold position" with a
	// liberation ETA interpolated from Retry-After. It keeps the bot in
	// voice even when the relay throttles.
	Busy = "The Lost Son is busy spreading democracy on Cyberstan. Hold position, Helldiver — try again shortly. Estimated time of liberation: %s."
	// Dead is the reply for every other backend failure. Short and final:
	// no fallback model exists, so the bot says so in character.
	Dead = "Uplink dead. No fallback. For Super Earth, try later."
	// Exhausted is the reply when the model keeps calling tools instead of
	// answering and falls off the maxToolRounds loop. The uplink is healthy
	// in this case — the intel is just thin — so it must never read as
	// Dead, or ops cannot triage from user reports.
	Exhausted = "Couldn't pin it down — intel thin or scattered. Unconfirmed. Narrow the target and try again."
)

// retryRe extracts an ETA number from error text like "retry after 12".
// It is only consulted after the 429/rate gate in busyOrDead passes, so a
// bare "retry 3 times" can never trigger the busy reply on its own.
var retryRe = regexp.MustCompile(`(?i)retry[^0-9]*([0-9]+)`)

// Message is one conversation turn in Responses-API shape.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ToolDef describes one function tool offered to the model. Parameters is a
// JSON Schema object (e.g. {"type":"object","properties":{...}}).
type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// outputPart is one content part inside a message item.
type outputPart struct {
	Type string
	Text string
}

// outputItem is one entry of a response's output array that the client acts
// on. Raw preserves function_call items verbatim so follow-up requests can
// echo them back (the API requires the exact object alongside outputs).
type outputItem struct {
	Type      string
	CallID    string `json:"call_id"`
	Name      string
	Arguments string
	Content   []outputPart
	Raw       json.RawMessage
}

// Client calls the relay. HTTP may be overridden in tests; otherwise the
// process default client is used.
type Client struct {
	// BaseURL is the relay root, e.g. https://opencode.ai/zen/go.
	BaseURL string
	// APIKey is sent as a Bearer token.
	APIKey string
	// Model is the Go model id (muse-spark-1.3-contributor).
	Model string
	// HTTP is the transport; nil means http.DefaultClient.
	HTTP *http.Client
}

// Chat sends one completion request and returns the model text, or a canned
// in-character reply on any failure. History is capped at the last 10
// non-empty turns (empty content would waste budget and confuse the relay),
// the query is always appended last, and session should be stable per
// conversation (thread ID) so the relay can route and cache by session.
// The request pins low reasoning effort with a 4096-token ceiling: the
// relay otherwise spends the whole budget thinking and returns nothing.
func (c *Client) Chat(ctx context.Context, system, query string, history []Message, session string) string {
	// Sliding window over non-empty turns: drop blanks, keep the tail.
	var input []Message
	for _, m := range history {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		input = append(input, m)
		if len(input) >= 10 {
			input = input[len(input)-10:]
		}
	}
	input = append(input, Message{Role: "user", Content: query})
	if strings.TrimSpace(session) == "" {
		session = "rouge-main"
	}
	// TrimSuffix avoids a doubled slash (//v1/responses 404s) when the base
	// URL is configured with a trailing slash.
	url := strings.TrimSuffix(c.BaseURL, "/") + "/v1/responses"
	// Marshal is infallible for this map shape (strings, numbers, slices —
	// no channels or funcs), so the error is dropped by construction.
	body, _ := json.Marshal(map[string]any{
		"model": c.Model, "instructions": system, "input": input,
		"max_output_tokens": 4096, "temperature": 0.6,
		"reasoning": map[string]any{"effort": "low"},
	})
	text := c.doRound(ctx, url, session, body)
	return text.reply
}

// maxResponseBytes caps relay response decoding at 1MB. Relay replies are
// small (a few KB); without a cap a misbehaving upstream could exhaust
// memory via the streaming decoder.
const maxResponseBytes = 1 << 20

func decodeResponse(resp *http.Response, v any) error {
	return json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(v)
}

// roundResult is one decoded Responses-API round: either a canned reply
// (ok=false) or actionable output items (ok=true). text carries narration
// co-present with tool calls — context for the next round, never an answer.
// retryable marks a well-formed but content-free Dead envelope: the only
// !ok Dead that ChatWithTools may retry once. Transport, decode, and
// backend-error Dead replies are never retryable, so a persistent outage
// costs one request per turn, not two.
type roundResult struct {
	reply     string
	ok        bool
	items     []outputItem
	text      string
	retryable bool
}

// doRound POSTs one request body and decodes the response. Transport,
// status and envelope failures resolve to Busy/Dead with ok=false. Final
// means no tool calls: message text with no function call behind it resolves
// to it with ok=false. Calls present means not final — ok=true, with any
// co-present text attached as context, so a narration-plus-call round never
// drops the call. The API envelope decides, nothing else.
func (c *Client) doRound(ctx context.Context, url, session string, body []byte) roundResult {
	// 60s wall clock per round; the caller adds its own overall deadline.
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return roundResult{reply: Dead}
	}
	// The custom UA and session header are Go-relay requirements, not
	// decoration: generic SDK user agents and missing sessions are rejected
	// or deprioritized by the relay's abuse monitoring.
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "rouge-automaton/1.0")
	req.Header.Set("x-opencode-session", session)
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	resp, err := httpc.Do(req)
	if err != nil {
		log.Printf("gorelay: transport error: %v", err)
		return roundResult{reply: busyOrDead(err.Error())}
	}
	defer resp.Body.Close()
	// 429 is the only status with its own reply: Retry-After becomes the
	// liberation ETA. The "s" suffix is appended only for a pure number —
	// the header may also arrive as an HTTP date, which is used verbatim.
	if resp.StatusCode == 429 {
		ra := strings.TrimSpace(resp.Header.Get("retry-after"))
		if ra == "" {
			ra = "unknown — stand by"
		} else if _, err := strconv.Atoi(ra); err == nil {
			ra += "s"
		}
		// Drain (bounded) before returning so the connection stays reusable
		// — this is the hottest path under throttle.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return roundResult{reply: fmt.Sprintf(Busy, ra)}
	}
	// Decode items twice: typed for dispatch, raw for verbatim echo of
	// function_call objects in follow-up requests.
	var raw struct {
		Type       string            `json:"type"`
		OutputText string            `json:"output_text"`
		Output     []json.RawMessage `json:"output"`
	}
	if err := decodeResponse(resp, &raw); err != nil {
		log.Printf("gorelay: decode error: %v", err)
		return roundResult{reply: Dead}
	}
	if raw.Type == "error" {
		// Provider-level error envelope (credits, model, upstream): logged
		// with status for ops, answered with Dead — never retried here.
		log.Printf("gorelay: backend error status=%d", resp.StatusCode)
		return roundResult{reply: Dead}
	}
	var items []outputItem
	for _, r := range raw.Output {
		var item outputItem
		if err := json.Unmarshal(r, &item); err != nil {
			continue
		}
		item.Raw = r
		items = append(items, item)
	}
	// Calls present means not final: they execute and the loop continues,
	// with co-present narration riding into history as context.
	var calls []outputItem
	for _, item := range items {
		if item.Type == "function_call" {
			calls = append(calls, item)
		}
	}
	if len(calls) > 0 {
		text := ""
		for _, item := range items {
			if item.Type != "message" {
				continue
			}
			for _, part := range item.Content {
				if part.Type == "output_text" && part.Text != "" {
					text = part.Text
					break
				}
			}
			if text != "" {
				break
			}
		}
		if text == "" {
			text = raw.OutputText
		}
		return roundResult{ok: true, items: items, text: text}
	}
	// Extraction order matters: the structured output[] array wins over the
	// top-level output_text convenience field when both are present.
	for _, item := range items {
		if item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "output_text" && part.Text != "" {
				return roundResult{reply: part.Text}
			}
		}
	}
	if raw.OutputText != "" {
		return roundResult{reply: raw.OutputText}
	}
	if len(items) > 0 {
		return roundResult{ok: true, items: items}
	}
	// Well-formed but content-free response: Dead, not an empty Discord post.
	// Retryable: a single blank envelope may be transient, so the tool loop
	// gets one retry on the first round. Hard failures above stay
	// non-retryable.
	return roundResult{reply: Dead, retryable: true}
}

// Probe checks model-backend reachability with a minimal 1-token request.
// It returns "clear" (200), "busy" (429), or "dead" (anything else) so
// /status can report uplink state without parsing a full answer.
func (c *Client) Probe(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url := strings.TrimSuffix(c.BaseURL, "/") + "/v1/responses"
	body, _ := json.Marshal(map[string]any{
		"model":             c.Model,
		"input":             []Message{{Role: "user", Content: "ping"}},
		"max_output_tokens": 1, "temperature": 0,
		"reasoning": map[string]any{"effort": "low"},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "dead"
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "rouge-automaton/1.0")
	req.Header.Set("x-opencode-session", "rouge-status")
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	resp, err := httpc.Do(req)
	if err != nil {
		if probeRateLimited(err.Error()) {
			return "busy"
		}
		return "dead"
	}
	// Drain (bounded) before close so the connection stays reusable —
	// same 1MB-cap reasoning as doRound, smaller budget for a probe.
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	switch resp.StatusCode {
	case 200:
		return "clear"
	case 429:
		return "busy"
	default:
		return "dead"
	}
}

// probeRateLimited classifies transport errors for Probe. It matches 429,
// "too many requests", and "rate limit"/"ratelimit" — but never a bare
// "rate", which overmatches ("separate", "desperate").
func probeRateLimited(s string) bool {
	low := strings.ToLower(s)
	return strings.Contains(low, "429") ||
		strings.Contains(low, "too many requests") ||
		strings.Contains(low, "rate limit") ||
		strings.Contains(low, "ratelimit")
}

// maxToolRounds caps the think-act loop so one Discord message can never
// turn into an unbounded relay session. Ten rounds cover loadout-style
// questions spanning 4 gear collections plus wiki fallback; exhaustion
// still resolves to Exhausted, just fires far less often.
const maxToolRounds = 10

// ChatWithTools runs the think-act loop: the model may call the offered
// tools (each executed by exec, which returns plain-text results) before
// answering. Message text with no tool call behind it ends the loop; text
// beside calls rides into history as context while the calls execute. Tool
// results that are errors stay inside the loop as tool outputs; relay
// failures resolve to the same canned replies as Chat. Loop exhaustion
// (maxToolRounds of tool calls with no final answer) resolves to Exhausted,
// never Dead, so a healthy uplink with thin intel cannot read as an outage.
// A content-free first round (empty envelope or reasoning-only items) gets
// one retry: transient empties on no-tool turns — e.g. identity greetings —
// must not surface as Dead on a single blank reply.
func (c *Client) ChatWithTools(ctx context.Context, system, query string, history []Message, session string, tools []ToolDef, exec func(ctx context.Context, name, args string) string) string {
	// Same sliding window as Chat, but as []any so raw function_call items
	// and outputs can be appended verbatim in later rounds.
	var input []any
	for _, m := range history {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		input = append(input, m)
		if len(input) >= 10 {
			input = input[len(input)-10:]
		}
	}
	input = append(input, Message{Role: "user", Content: query})
	if strings.TrimSpace(session) == "" {
		session = "rouge-main"
	}
	url := strings.TrimSuffix(c.BaseURL, "/") + "/v1/responses"
	// Non-nil so the wire shape is [] on the tool-free path, never null.
	specs := []any{}
	for _, t := range tools {
		specs = append(specs, map[string]any{
			"type": "function", "name": t.Name,
			"description": t.Description, "parameters": t.Parameters,
		})
	}
	for round := 0; round < maxToolRounds; round++ {
		// Same infallible-shape guarantee as Chat: no error to handle.
		body, _ := json.Marshal(map[string]any{
			"model": c.Model, "instructions": system, "input": input,
			"tools":             specs,
			"max_output_tokens": 4096, "temperature": 0.6,
			"reasoning": map[string]any{"effort": "low"},
		})
		res := c.doRound(ctx, url, session, body)
		if round == 0 && isEmptyFirstRound(res) && ctx.Err() == nil {
			// One retry for no-tool turns that come back empty: a single
			// blank envelope may be transient and must not fail an
			// answerable question. Only content-free empties land here —
			// Busy and hard-error Dead replies are never retryable, so
			// throttled or broken backends still return after one request.
			log.Printf("gorelay: empty first round, retrying session=%s query=%q", session, trunc(query, 120))
			res = c.doRound(ctx, url, session, body)
		}
		if !res.ok {
			// First text ends the loop, always — no ack detection. Telling
			// a stranded ack from a greeting takes content judgment, and
			// neither a verb list nor a second model call earns its keep:
			// the prompt already orders "function call or final answer",
			// and every question now has a valid tool (war_status closed
			// the gap that stranded the production incident).
			return res.reply
		}
		var calls []outputItem
		for _, item := range res.items {
			if item.Type == "function_call" {
				calls = append(calls, item)
			}
		}
		if res.text != "" {
			// Narration co-present with calls is context, never the answer:
			// verbatim into history (model-facing only — Sanitize runs
			// solely on the final reply path).
			input = append(input, Message{Role: "assistant", Content: res.text})
		}
		if len(calls) == 0 {
			// No text either (doRound would have returned it): stop
			// rather than spin on content-free rounds. Genuine relay
			// failure, so Dead — distinct from loop exhaustion below.
			log.Printf("gorelay: content-free tool round session=%s round=%d query=%q", session, round, trunc(query, 120))
			return Dead
		}
		if exec == nil {
			// Tool-free path (greetings) offered no tools, so a function
			// call is unexpected: Dead, never a nil-executor panic.
			log.Printf("gorelay: unexpected tool call with nil executor")
			return Dead
		}
		for _, call := range calls {
			log.Printf("gorelay: tool %s(%s)", call.Name, trunc(call.Arguments, 120))
			out := exec(ctx, call.Name, call.Arguments)
			input = append(input, json.RawMessage(call.Raw))
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": call.CallID, "output": out,
			})
		}
	}
	// Fell off the loop with tool calls every round and no final answer.
	// The backends are reachable here — the model just never answered — so
	// Exhausted, never Dead. Logged with session + query so the trigger
	// stops being invisible in ops triage.
	log.Printf("gorelay: tool loop exhausted session=%s query=%q", session, trunc(query, 120))
	return Exhausted
}

// isEmptyFirstRound reports whether res carries no answer and no tool call
// and is safe to retry once: a retryable content-free Dead envelope (!ok
// with retryable set in doRound — Busy and hard-error Dead excluded) or an
// ok round with neither text nor function calls (reasoning-only items).
// Only the first round qualifies for the retry in ChatWithTools.
func isEmptyFirstRound(res roundResult) bool {
	if !res.ok {
		return res.retryable
	}
	if res.text != "" {
		return false
	}
	for _, item := range res.items {
		if item.Type == "function_call" {
			return false
		}
	}
	return true
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// numRe guards ETA extraction to pure numbers.
var numRe = regexp.MustCompile(`^[0-9]+$`)

// busyOrDead classifies transport failures. The gate requires an explicit
// 429/rate-limit signal before the retry regex is even consulted — this
// kills both false positives ("retry 3 times" without throttling) and false
// negatives (a bare 429 with no number, which gets the stand-by ETA).
func busyOrDead(s string) string {
	// probeRateLimited is the single tight matcher (429, rate limit,
	// ratelimit, too many requests) — bare "rate" must never qualify
	// ("separate", "desperate").
	if probeRateLimited(strings.ToLower(s)) {
		if m := retryRe.FindStringSubmatch(s); m != nil && numRe.MatchString(m[1]) {
			return fmt.Sprintf(Busy, m[1]+"s")
		}
		return fmt.Sprintf(Busy, "unknown — stand by")
	}
	return Dead
}
