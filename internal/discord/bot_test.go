package discord

import "testing"

// cutRunes must cut by characters, never splitting multibyte runes, and
// pass short strings through untouched.
func TestCutRunes(t *testing.T) {
	if got := cutRunes("héllo—world", 5); got != "héllo" {
		t.Fatalf("rune cut: %q", got)
	}
	if cutRunes("hi", 5) != "hi" {
		t.Fatal("short string must pass through")
	}
}
