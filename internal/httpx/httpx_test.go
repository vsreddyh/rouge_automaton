package httpx

import "testing"

func TestBodySnippet(t *testing.T) {
	if got := BodySnippet([]byte("  error code: 502\n")); got != "error code: 502" {
		t.Fatalf("snippet = %q", got)
	}
	if got := BodySnippet(nil); got != "" {
		t.Fatalf("snippet = %q, want empty", got)
	}
	long := make([]byte, 400)
	for i := range long {
		long[i] = 'x'
	}
	if got := BodySnippet(long); len(got) != maxSnippet {
		t.Fatalf("snippet length = %d, want %d", len(got), maxSnippet)
	}
}

func TestBodySuffix(t *testing.T) {
	if got := BodySuffix([]byte("rate limited")); got != ": rate limited" {
		t.Fatalf("suffix = %q", got)
	}
	if got := BodySuffix(nil); got != "" {
		t.Fatalf("suffix = %q, want empty", got)
	}
}
