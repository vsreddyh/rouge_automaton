package gorelay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-opencode-session") == "" {
			t.Error("session header required by Go relay rules")
		}
		if r.Header.Get("User-Agent") == "Go-http-client/1.1" {
			t.Error("generic SDK user agent rejected by Go relay rules")
		}
		w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"Hulk. Rear vent. Over."}]}]}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, APIKey: "k", Model: "m", HTTP: srv.Client()}
	if got := c.Chat(context.Background(), "sys", "q", nil, "s1"); got != "Hulk. Rear vent. Over." {
		t.Fatalf("got %q", got)
	}
}

func TestErrors(t *testing.T) {
	srv429 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
	}))
	defer srv429.Close()
	c := &Client{BaseURL: srv429.URL, HTTP: srv429.Client()}
	if got := c.Chat(context.Background(), "", "", nil, "s"); !strings.Contains(got, "7s") {
		t.Fatalf("429 must carry liberation ETA: %q", got)
	}
	srv429bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
	}))
	defer srv429bare.Close()
	cbare := &Client{BaseURL: srv429bare.URL, HTTP: srv429bare.Client()}
	if got := cbare.Chat(context.Background(), "", "", nil, "s"); !strings.Contains(got, "unknown — stand by") {
		t.Fatalf("429 without header must use fallback ETA: %q", got)
	}
	srvErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"error","error":{"type":"CreditsError"}}`))
	}))
	defer srvErr.Close()
	c2 := &Client{BaseURL: srvErr.URL, HTTP: srvErr.Client()}
	if got := c2.Chat(context.Background(), "", "", nil, "s"); got != Dead {
		t.Fatalf("backend error must be Dead: %q", got)
	}
	srvBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer srvBad.Close()
	c3 := &Client{BaseURL: srvBad.URL, HTTP: srvBad.Client()}
	if got := c3.Chat(context.Background(), "", "", nil, "s"); got != Dead {
		t.Fatalf("invalid JSON must be Dead: %q", got)
	}
}

func TestPrecedenceAndShape(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v struct {
			Input []Message `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&v)
		for _, m := range v.Input {
			if strings.TrimSpace(m.Content) == "" {
				t.Error("empty history content must be filtered")
			}
			body = m.Content
		}
		// Both shapes present: output[] must win.
		w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"from-output"}]}],"output_text":"from-toplevel"}`))
	}))
	defer srv.Close()
	// Trailing slash must not double up the path.
	c := &Client{BaseURL: srv.URL + "/", HTTP: srv.Client()}
	hist := []Message{{Role: "user", Content: ""}, {Role: "user", Content: "q"}}
	if got := c.Chat(context.Background(), "", "q2", hist, ""); got != "from-output" {
		t.Fatalf("output[] must take precedence: %q (last input %q)", got, body)
	}
	if body != "q2" {
		t.Fatalf("last input must be the query, got %q", body)
	}
}

func TestBusyGate(t *testing.T) {
	if got := busyOrDead("429 Too Many Requests"); !strings.Contains(got, "unknown — stand by") {
		t.Fatalf("bare 429 must be busy: %q", got)
	}
	if got := busyOrDead("please retry 3 times later"); got != Dead {
		t.Fatalf("bare retry count without 429/rate must be Dead: %q", got)
	}
	if got := busyOrDead("rate limited, retry after 12"); !strings.Contains(got, "12s") {
		t.Fatalf("rate+number must carry ETA: %q", got)
	}
}

func TestChatWithTools(t *testing.T) {
	var bodies []string
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v struct {
			Input []any `json:"input"`
			Tools []any `json:"tools"`
		}
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		json.Unmarshal(raw, &v)
		if len(v.Tools) == 0 {
			t.Error("tools must be sent every round")
		}
		round++
		if round == 1 {
			w.Write([]byte(`{"output":[{"type":"function_call","call_id":"call_1","name":"search_units","arguments":"{\"query\":\"hulk\"}"}]}`))
			return
		}
		// Round 2 must echo the call and carry its output.
		joined := string(raw)
		if !strings.Contains(joined, "call_1") || !strings.Contains(joined, "Back vent") {
			t.Errorf("round 2 must echo call + output, got %s", joined[:200])
		}
		w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"Hulk. Rear vent. Over."}]}]}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	execCalls := 0
	got := c.ChatWithTools(context.Background(), "", "kill hulk?", nil, "s",
		[]ToolDef{{Name: "search_units"}},
		func(ctx context.Context, name, args string) string {
			execCalls++
			if name != "search_units" || !strings.Contains(args, "hulk") {
				t.Errorf("exec got %q %q", name, args)
			}
			return "Hulk Bruiser: weak Back vent"
		})
	if got != "Hulk. Rear vent. Over." {
		t.Fatalf("got %q", got)
	}
	if execCalls != 1 || len(bodies) != 2 {
		t.Fatalf("exec=%d rounds=%d", execCalls, len(bodies))
	}
}

func TestChatWithToolsNoCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"Hi. Over."}]}]}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	called := false
	got := c.ChatWithTools(context.Background(), "", "hi", nil, "s", nil,
		func(ctx context.Context, name, args string) string { called = true; return "" })
	if got != "Hi. Over." || called {
		t.Fatalf("passthrough failed: %q called=%v", got, called)
	}
}
