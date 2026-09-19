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
// The request pins low reasoning effort with a 1024-token ceiling: the
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
		"max_output_tokens": 1024, "temperature": 0.6,
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
// (ok=false) or actionable output items (ok=true).
type roundResult struct {
	reply string
	ok    bool
	items []outputItem
}

// doRound POSTs one request body and decodes the response. Transport,
// status and envelope failures resolve to Busy/Dead with ok=false; a
// decodable response with message text resolves to it with ok=false; a
// response carrying only non-message items (e.g. function calls) resolves
// with ok=true so the caller can act on them.
func (c *Client) doRound(ctx context.Context, url, session string, body []byte) roundResult {
	// 60s wall clock per the terse reply budget; the caller adds its own.
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
	return roundResult{reply: Dead}
}

// maxToolRounds caps the think-act loop so one Discord message can never
// turn into an unbounded relay session.
const maxToolRounds = 3

// ChatWithTools runs the think-act loop: the model may call the offered
// tools (each executed by exec, which returns plain-text results) before
// answering. The first message text ends the loop. Tool results that are
// errors stay inside the loop as tool outputs; relay failures resolve to
// the same canned replies as Chat.
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
			"max_output_tokens": 1024, "temperature": 0.6,
			"reasoning": map[string]any{"effort": "low"},
		})
		res := c.doRound(ctx, url, session, body)
		if !res.ok {
			return res.reply
		}
		var calls []outputItem
		for _, item := range res.items {
			if item.Type == "function_call" {
				calls = append(calls, item)
			}
		}
		if len(calls) == 0 {
			// No text either (doRound would have returned it): stop
			// rather than spin on content-free rounds.
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
	return Dead
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
	low := strings.ToLower(s)
	if strings.Contains(low, "429") || strings.Contains(low, "rate") ||
		strings.Contains(low, "too many requests") {
		if m := retryRe.FindStringSubmatch(s); m != nil && numRe.MatchString(m[1]) {
			return fmt.Sprintf(Busy, m[1]+"s")
		}
		return fmt.Sprintf(Busy, "unknown — stand by")
	}
	return Dead
}
