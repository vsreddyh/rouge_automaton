package discord

import (
	"strings"
	"testing"
)

func TestPickContext(t *testing.T) {
	wikiCalled := false
	wiki := func() string { wikiCalled = true; return "wiki-doc" }

	// Docs + live: both present, wiki untouched.
	if got := pickContext("doc", 1, "live", wiki); got != "doc\nlive" || wikiCalled {
		t.Fatalf("docs+live: %q called=%v", got, wikiCalled)
	}
	// No docs, no live: wiki fires.
	if got := pickContext("", 0, "", wiki); got != "wiki-doc" || !wikiCalled {
		t.Fatalf("empty: %q called=%v", got, wikiCalled)
	}
	// L + live miss + no docs: wiki fires (the #9/#10 gating case).
	wikiCalled = false
	if got := pickContext("", 0, "", wiki); got != "wiki-doc" {
		t.Fatalf("live-miss: %q", got)
	}
	// L + live hit, no docs: live only, wiki untouched.
	wikiCalled = false
	if got := pickContext("", 0, "Mintoria: Humans", wiki); got != "Mintoria: Humans" || wikiCalled {
		t.Fatalf("live-hit: %q called=%v", got, wikiCalled)
	}
}

func TestCutRunes(t *testing.T) {
	if got := cutRunes("héllo—world", 5); got != "héllo" {
		t.Fatalf("rune cut: %q", got)
	}
	if cutRunes("hi", 5) != "hi" {
		t.Fatal("short string must pass through")
	}
	thread := cutRunes("line1\nline2 which is long", 60)
	if !strings.Contains(thread, "\n") {
		t.Fatal("cutRunes must not strip newlines (caller sanitizes)")
	}
}
