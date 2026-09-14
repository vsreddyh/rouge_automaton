package zen

import (
	"context"
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
	srvErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"error","error":{"type":"CreditsError"}}`))
	}))
	defer srvErr.Close()
	c2 := &Client{BaseURL: srvErr.URL, HTTP: srvErr.Client()}
	if got := c2.Chat(context.Background(), "", "", nil, "s"); got != Dead {
		t.Fatalf("backend error must be Dead: %q", got)
	}
}
