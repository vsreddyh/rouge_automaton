package persona

import (
	"strings"
	"testing"
)

func TestPostprocess(t *testing.T) {
	if got := Postprocess("Hulk. Rear vent."); !strings.HasSuffix(got, "Over.") {
		t.Fatalf("missing sign-off: %q", got)
	}
	if got := Postprocess("serial 12345"); strings.Contains(got, "12345") {
		t.Fatalf("serial leaked: %q", got)
	}
	if got := Postprocess("Done. Over."); got != "Done. Over." {
		t.Fatalf("stable reply rewritten: %q", got)
	}
	// Terminal punctuation we did not write must survive the sign-off:
	// "Attack!" joins with a space, periods collapse to one ". Over.".
	if got := Postprocess("Attack!"); got != "Attack! Over." {
		t.Fatalf("exclamation mangled: %q", got)
	}
	if got := Postprocess("Hulk. Rear vent."); got != "Hulk. Rear vent. Over." {
		t.Fatalf("period join wrong: %q", got)
	}
}

func TestSystemPrompt(t *testing.T) {
	// Suffix alone contains "Helldiver", so assert on SOUL-specific canon.
	if got := SystemPrompt(); !strings.Contains(got, "Lost Son of Managed Democracy") {
		t.Fatal("system prompt must carry SOUL.md canon")
	}
	t.Setenv("SKILLS_DIR", t.TempDir())
	if got := SystemPrompt(); !strings.Contains(got, "Lost Son of Managed Democracy") {
		t.Fatal("invalid SKILLS_DIR must fall back to embedded SOUL.md")
	}
}
