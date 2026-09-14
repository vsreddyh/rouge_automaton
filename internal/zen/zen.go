// Package zen talks to the OpenCode Go relay over its Responses API
// (POST {base}/v1/responses). It is the bot's only model backend: there is
// no fallback model, so every failure path resolves to an in-character
// canned reply instead of a downgrade.
package zen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	body, _ := json.Marshal(map[string]any{
		"model": c.Model, "instructions": system, "input": input,
		"max_output_tokens": 1024, "temperature": 0.6,
		"reasoning": map[string]any{"effort": "low"},
	})
	// 60s wall clock per the terse reply budget; the caller adds its own.
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return Dead
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
		log.Printf("zen: transport error: %v", err)
		return busyOrDead(err.Error())
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
		return fmt.Sprintf(Busy, ra)
	}
	var data struct {
		Type       string `json:"type"`
		OutputText string `json:"output_text"`
		Output     []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Printf("zen: decode error: %v", err)
		return Dead
	}
	if data.Type == "error" {
		// Provider-level error envelope (credits, model, upstream): logged
		// with status for ops, answered with Dead — never retried here.
		log.Printf("zen: backend error status=%d", resp.StatusCode)
		return Dead
	}
	// Extraction order matters: the structured output[] array wins over the
	// top-level output_text convenience field when both are present.
	for _, item := range data.Output {
		if item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "output_text" && part.Text != "" {
				return part.Text
			}
		}
	}
	if data.OutputText != "" {
		return data.OutputText
	}
	// Well-formed but content-free response: Dead, not an empty Discord post.
	return Dead
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
