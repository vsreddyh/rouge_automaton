// Package zen talks to the OpenCode Go relay (Responses API).
// Ports bot/llm.py: terse budgets, 429 busy reply, no silent fallback.
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

// Busy is the 429 reply; Dead covers every other backend failure.
const (
	Busy = "The Lost Son is busy spreading democracy on Cyberstan. Hold position, Helldiver — try again shortly. Estimated time of liberation: %s."
	Dead = "Uplink dead. No fallback. For Super Earth, try later."
)

var retryRe = regexp.MustCompile(`(?i)retry[^0-9]*([0-9]+)`)

// Message is one conversation turn.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client calls the relay.
type Client struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client
}

// Chat sends system instructions + history + query, returns model text or a
// canned failure reply. Session should be stable per conversation (thread ID).
func (c *Client) Chat(ctx context.Context, system, query string, history []Message, session string) string {
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
	url := strings.TrimSuffix(c.BaseURL, "/") + "/v1/responses"
	body, _ := json.Marshal(map[string]any{
		"model": c.Model, "instructions": system, "input": input,
		"max_output_tokens": 1024, "temperature": 0.6,
		"reasoning": map[string]any{"effort": "low"},
	})
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return Dead
	}
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
		log.Printf("zen: backend error status=%d", resp.StatusCode)
		return Dead
	}
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
	return Dead
}

var numRe = regexp.MustCompile(`^[0-9]+$`)

// busyOrDead maps transport failures: 429/rate-limit mentions route to the
// busy reply (with ETA when a number is present), everything else is Dead.
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
