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

// backstoryKeywords are the biography terms that must never reach the model
// unprompted on the intel path.
var backstoryKeywords = []string{"Cyberstan", "lab rat", "husk", "torture", "captured"}

func TestIntelPromptHasNoBackstory(t *testing.T) {
	got := IntelSystemPrompt()
	for _, kw := range backstoryKeywords {
		if strings.Contains(strings.ToLower(got), strings.ToLower(kw)) {
			t.Fatalf("intel prompt leaks backstory %q", kw)
		}
	}
	if !strings.Contains(got, "Helldiver") || !strings.Contains(got, "Over.") {
		t.Fatal("intel prompt must keep voice card (Helldiver, Over.)")
	}
	if c := len(strings.Fields(VoiceCard)); c >= 120 {
		t.Fatalf("voice card must stay <120 words, got %d", c)
	}
	intelQs := []string{
		"how to kill Hulk?", "best loadout vs Terminids",
		"planet status Mintoria", "hi", "where to fight tundra",
	}
	for _, q := range intelQs {
		if IsIdentityQuestion(q) {
			t.Fatalf("intel query misgated as identity: %q", q)
		}
		if got := PromptFor(q); strings.Contains(got, "lab rat") {
			t.Fatalf("intel PromptFor leaks canon for %q", q)
		}
	}
}

func TestIdentityPromptKeepsCanon(t *testing.T) {
	for _, q := range []string{
		"who are you?", "what is your name?", "your serial?",
		"tell me about Brasch", "what happened to you on Cyberstan?",
	} {
		if !IsIdentityQuestion(q) {
			t.Fatalf("identity query missed: %q", q)
		}
		got := PromptFor(q)
		if !strings.Contains(got, "[REDACTED]") {
			t.Fatalf("identity prompt must carry serial canon for %q", q)
		}
	}
	if IsIdentityQuestion("") {
		t.Fatal("empty query must gate to intel")
	}
}
