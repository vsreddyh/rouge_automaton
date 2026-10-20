package gorelay

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
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

// TestChatWithToolsAckPassthrough pins the no-tripwire loop: round 1 text
// with no function call behind it is the final answer, even when it reads
// like a lookup ack. Telling that from a greeting takes content judgment;
// the prompt ("function call or final answer") plus a valid tool for every
// question is the whole defense now.
func TestChatWithToolsAckPassthrough(t *testing.T) {
	rounds := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rounds++
		w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"Checking Fury's war status. Over."}]}]}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	called := false
	got := c.ChatWithTools(context.Background(), "", "status of fury?", nil, "s",
		[]ToolDef{{Name: "planet_status"}},
		func(ctx context.Context, name, args string) string { called = true; return "" })
	if got != "Checking Fury's war status. Over." {
		t.Fatalf("first text must pass through as final, got %q", got)
	}
	if called || rounds != 1 {
		t.Fatalf("no tool round must follow text: called=%v rounds=%d", called, rounds)
	}
}

// TestChatWithToolsMixedRound pins the envelope rule: text beside a function
// call is context, not final — the call executes and the loop continues,
// instead of posting the narration and dropping the intel.
func TestChatWithToolsMixedRound(t *testing.T) {
	var bodies []string
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		round++
		if round == 1 {
			w.Write([]byte(`{"output":[` +
				`{"type":"message","content":[{"type":"output_text","text":"On it, pulling the line. Over."}]},` +
				`{"type":"function_call","call_id":"call_1","name":"planet_status","arguments":"{\"name\":\"FURY\"}"}` +
				`]}`))
			return
		}
		w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"FURY: Humans 0.0% — 0 divers. Over."}]}]}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	execCalls := 0
	got := c.ChatWithTools(context.Background(), "", "status of fury?", nil, "s",
		[]ToolDef{{Name: "planet_status"}},
		func(ctx context.Context, name, args string) string {
			execCalls++
			return "FURY: Humans 0.0% — 0 divers. Over."
		})
	if got != "FURY: Humans 0.0% — 0 divers. Over." {
		t.Fatalf("mixed round must complete the tool call, got %q", got)
	}
	if execCalls != 1 || len(bodies) != 2 {
		t.Fatalf("exec=%d rounds=%d, want 1/2", execCalls, len(bodies))
	}
	if !strings.Contains(bodies[1], "On it, pulling the line.") {
		t.Fatalf("round 2 must carry the narration as context: %s", bodies[1][:200])
	}
	if !strings.Contains(bodies[1], "function_call_output") {
		t.Fatalf("round 2 must carry the tool output: %s", bodies[1][:200])
	}
}

// TestChatWithToolsNilExec locks in the tool-free path: an unexpected
// function call with a nil executor must resolve to Dead, never panic.
func TestChatWithToolsNilExec(t *testing.T) {
	rounds := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rounds++
		var v struct {
			Tools []any `json:"tools"`
		}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &v)
		if v.Tools == nil {
			t.Error("tools must marshal as [], never null")
		}
		w.Write([]byte(`{"output":[{"type":"function_call","call_id":"call_9","name":"search_units","arguments":"{}"}]}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if got := c.ChatWithTools(context.Background(), "", "hi", nil, "s", nil, nil); got != Dead {
		t.Fatalf("nil exec must be Dead: %q", got)
	}
	if rounds != 1 {
		t.Fatalf("nil exec must stop after one round, got %d", rounds)
	}
}

// TestChatWithToolsExhausted pins issue #43 path (c): maxToolRounds of tool
// rounds with no final answer must resolve to Exhausted, never Dead, so a
// healthy uplink with thin intel cannot read as an outage.
func TestChatWithToolsExhausted(t *testing.T) {
	rounds := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rounds++
		w.Write([]byte(`{"output":[{"type":"function_call","call_id":"call_` + strconv.Itoa(rounds) + `","name":"search_gear","arguments":"{}"}]}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	got := c.ChatWithTools(context.Background(), "", "best loadout vs Automatons?", nil, "s",
		[]ToolDef{{Name: "search_gear"}},
		func(ctx context.Context, name, args string) string { return "some intel" })
	if got != Exhausted {
		t.Fatalf("exhaustion must be Exhausted, got %q", got)
	}
	if got == Dead {
		t.Fatalf("exhaustion must never read as Dead")
	}
	if rounds != maxToolRounds {
		t.Fatalf("must run all %d tool rounds, got %d", maxToolRounds, rounds)
	}
}

// TestChatWithToolsEmptyFirstRoundRetry pins issue #43 path (2): a
// content-free first round on a no-tool turn gets one retry, so a single
// blank envelope cannot fail an answerable greeting as Dead.
func TestChatWithToolsEmptyFirstRoundRetry(t *testing.T) {
	rounds := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rounds++
		if rounds == 1 {
			w.Write([]byte(`{"output":[]}`))
			return
		}
		w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"Hi. Over."}]}]}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	got := c.ChatWithTools(context.Background(), "", "who are you?", nil, "s", nil,
		func(ctx context.Context, name, args string) string { return "" })
	if got != "Hi. Over." {
		t.Fatalf("empty first round must retry to an answer, got %q", got)
	}
	if rounds != 2 {
		t.Fatalf("must retry once after an empty first round, got %d rounds", rounds)
	}
}

// TestChatWithToolsBusyNoRetry guards the retry: a throttled first round
// resolves to Busy immediately with no second request.
func TestChatWithToolsBusyNoRetry(t *testing.T) {
	rounds := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rounds++
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	got := c.ChatWithTools(context.Background(), "", "who are you?", nil, "s", nil,
		func(ctx context.Context, name, args string) string { return "" })
	if !strings.Contains(got, "7s") {
		t.Fatalf("throttled turn must be Busy, got %q", got)
	}
	if rounds != 1 {
		t.Fatalf("busy must not retry, got %d rounds", rounds)
	}
}

// TestChatWithToolsDoubleEmptyNoSecondRetry bounds the retry: two blank
// envelopes in a row still resolve to Dead after exactly one retry.
func TestChatWithToolsDoubleEmptyNoSecondRetry(t *testing.T) {
	rounds := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rounds++
		w.Write([]byte(`{"output":[]}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	got := c.ChatWithTools(context.Background(), "", "who are you?", nil, "s", nil,
		func(ctx context.Context, name, args string) string { return "" })
	if got != Dead {
		t.Fatalf("double-empty must stay Dead, got %q", got)
	}
	if rounds != 2 {
		t.Fatalf("must retry exactly once, got %d rounds", rounds)
	}
}

// TestChatWithToolsReasoningOnlyRetry covers the ok-shaped empty: a
// reasoning-only first round (items, no text, no calls) retries once.
func TestChatWithToolsReasoningOnlyRetry(t *testing.T) {
	rounds := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rounds++
		if rounds == 1 {
			w.Write([]byte(`{"output":[{"type":"reasoning","call_id":"","name":""}]}`))
			return
		}
		w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"Hi. Over."}]}]}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	got := c.ChatWithTools(context.Background(), "", "who are you?", nil, "s", nil,
		func(ctx context.Context, name, args string) string { return "" })
	if got != "Hi. Over." {
		t.Fatalf("reasoning-only first round must retry to an answer, got %q", got)
	}
	if rounds != 2 {
		t.Fatalf("must retry once after a reasoning-only round, got %d rounds", rounds)
	}
}

// TestChatWithToolsHardErrorNoRetry guards the retryable flag: a backend
// error envelope is Dead but never retried, so a persistent outage costs
// one request per turn, not two.
func TestChatWithToolsHardErrorNoRetry(t *testing.T) {
	rounds := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rounds++
		w.Write([]byte(`{"type":"error","error":{"type":"CreditsError"}}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	got := c.ChatWithTools(context.Background(), "", "who are you?", nil, "s", nil,
		func(ctx context.Context, name, args string) string { return "" })
	if got != Dead {
		t.Fatalf("backend error must be Dead, got %q", got)
	}
	if rounds != 1 {
		t.Fatalf("hard-error Dead must not retry, got %d rounds", rounds)
	}
}

func TestProbe(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   string
	}{
		{"ok", 200, "clear"},
		{"throttled", 429, "busy"},
		{"broke", 500, "dead"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("x-opencode-session") != "rouge-status" {
				t.Error("probe must carry the rouge-status session")
			}
			w.WriteHeader(c.status)
		}))
		cl := &Client{BaseURL: srv.URL, APIKey: "k", Model: "m", HTTP: srv.Client()}
		if got := cl.Probe(context.Background()); got != c.want {
			t.Fatalf("%s: got %q want %q", c.name, got, c.want)
		}
		srv.Close()
	}
}

func TestProbeRateLimited(t *testing.T) {
	for _, s := range []string{"429 too many", "Rate limit hit", "ratelimited", "too many requests"} {
		if !probeRateLimited(s) {
			t.Fatalf("must read busy: %q", s)
		}
	}
	for _, s := range []string{"separate failure", "desperate times", "connection reset"} {
		if probeRateLimited(s) {
			t.Fatalf("must not read busy: %q", s)
		}
	}
}

// TestOutputTokenCeiling pins issue #44 §3: Chat and ChatWithTools request
// 4096 output tokens (fuller answers allowed), while Probe stays at 1.
func TestOutputTokenCeiling(t *testing.T) {
	check := func(t *testing.T, path string, call func(c *Client), want float64) {
		t.Helper()
		var seen atomic.Uint64 // math.Float64bits, race-safe across handler/test goroutines
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var v struct {
				MaxTokens float64 `json:"max_output_tokens"`
			}
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("%s: reading request body: %v", path, err)
			}
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Errorf("%s: decoding request body: %v", path, err)
			}
			seen.Store(math.Float64bits(v.MaxTokens))
			if path == "probe" {
				w.WriteHeader(200)
				return
			}
			_, _ = w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"Hi. Over."}]}]}`))
		}))
		defer srv.Close()
		c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
		call(c)
		if got := math.Float64frombits(seen.Load()); got != want {
			t.Fatalf("%s: max_output_tokens = %v, want %v", path, got, want)
		}
	}
	check(t, "chat", func(c *Client) {
		c.Chat(context.Background(), "", "hi", nil, "s")
	}, 4096)
	check(t, "tools", func(c *Client) {
		c.ChatWithTools(context.Background(), "", "hi", nil, "s", nil,
			func(ctx context.Context, name, args string) string { return "" })
	}, 4096)
	check(t, "probe", func(c *Client) { c.Probe(context.Background()) }, 1)
}
