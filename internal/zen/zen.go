// Package zen talks to the OpenCode Go relay (Responses API).
// Ports bot/llm.py: terse budgets, 429 busy reply, no silent fallback.
package zen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
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
	if len(history) > 10 {
		history = history[len(history)-10:]
	}
	input := append(append([]Message{}, history...), Message{Role: "user", Content: query})
	body, _ := json.Marshal(map[string]any{
		"model": c.Model, "instructions": system, "input": input,
		"max_output_tokens": 1024, "temperature": 0.6,
		"reasoning": map[string]any{"effort": "low"},
	})
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+"/v1/responses", bytes.NewReader(body))
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
		return busyOrDead(err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		ra := resp.Header.Get("retry-after")
		if ra == "" {
			ra = "unknown — stand by"
		}
		return fmt.Sprintf(Busy, ra+"s")
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
		return Dead
	}
	if data.Type == "error" {
		return Dead
	}
	if data.OutputText != "" {
		return data.OutputText
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
	return Dead
}

func busyOrDead(s string) string {
	if m := retryRe.FindStringSubmatch(s); m != nil {
		return fmt.Sprintf(Busy, m[1]+"s")
	}
	return Dead
}
