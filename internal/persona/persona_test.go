package persona

import (
	"strings"
	"testing"
)

func TestGreetings(t *testing.T) {
	for _, g := range []string{"hi", "Hello", "HEY", "o7"} {
		if !IsGreeting(g) {
			t.Fatalf("%q should be a greeting", g)
		}
	}
	if IsGreeting("how to kill hulk") {
		t.Fatal("intel query must not be a greeting")
	}
}

func TestPostprocess(t *testing.T) {
	if got := Postprocess("hello there\nsecond line", true); strings.Contains(got, "\n") {
		t.Fatalf("greeting must be one line: %q", got)
	}
	if got := Postprocess("Hulk. Rear vent.", false); !strings.HasSuffix(got, "Over.") {
		t.Fatalf("missing sign-off: %q", got)
	}
	if got := Postprocess("serial 12345", false); strings.Contains(got, "12345") {
		t.Fatalf("serial leaked: %q", got)
	}
	if got := Postprocess("Done. Over.", false); got != "Done. Over." {
		t.Fatalf("stable reply rewritten: %q", got)
	}
}

func TestSystemPrompt(t *testing.T) {
	if !strings.Contains(SystemPrompt(), "Helldiver") {
		t.Fatal("system prompt must carry the SOUL voice")
	}
}
