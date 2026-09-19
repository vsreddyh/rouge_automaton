package discord

import "testing"

// gatePass must admit exactly the intended matrix: open mode admits all,
// otherwise one of mention, free channel, or live thread session is needed.
func TestGatePass(t *testing.T) {
	cases := []struct {
		name                           string
		require, mentioned, free, live bool
		want                           bool
	}{
		{"open mode admits stranger chatter", false, false, false, false, true},
		{"mention admits", true, true, false, false, true},
		{"free channel admits", true, false, true, false, true},
		{"live thread admits", true, false, false, true, true},
		{"plain channel denies", true, false, false, false, false},
		{"all signals admits", true, true, true, true, true},
	}
	for _, c := range cases {
		if got := gatePass(c.require, c.mentioned, c.free, c.live); got != c.want {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// threadSet must be zero-value ready, drop empty IDs, and hit tracked ones.
func TestThreadSet(t *testing.T) {
	var ts threadSet
	if ts.has("nope") {
		t.Fatal("empty set must miss")
	}
	ts.add("")
	if ts.has("") {
		t.Fatal("empty ID must never hit")
	}
	ts.add("thread-1")
	if !ts.has("thread-1") {
		t.Fatal("tracked thread must hit")
	}
}

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
