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
	if got := Postprocess("serial-number 12345"); strings.Contains(got, "12345") {
		t.Fatalf("hyphenated serial leaked: %q", got)
	}
	if got := Postprocess("serial: #7"); strings.Contains(got, "7") {
		t.Fatalf("colon serial leaked: %q", got)
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
	if got := Postprocess(""); got != "Over." {
		t.Fatalf("empty reply must be bare sign-off: %q", got)
	}
	if got := Postprocess("Standing over."); got != "Standing over." {
		t.Fatalf("lowercase sign-off must not duplicate: %q", got)
	}
	if got := Postprocess("Flying the layover."); got != "Flying the layover. Over." {
		t.Fatalf("layover must not read as sign-off: %q", got)
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
	if !strings.Contains(got, "never refuse") {
		t.Fatal("intel prompt must keep the equal-faction never-refuse rule")
	}
	if c := len(strings.Fields(VoiceCard)); c >= 120 {
		t.Fatalf("voice card must stay <120 words, got %d", c)
	}
	intelQs := []string{
		"how to kill Hulk?", "best loadout vs Terminids",
		"planet status Mintoria", "hi", "where to fight tundra",
		// Regression: open-ended phrasing must not open the canon gate.
		"what are you best vs hulks?", "what is my serial?",
		"serial number of the Hulk?", "what are you supposed to use vs hulk?",
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
		"who are you?", "what is your name?", "your serial?", "serial?",
		"what are you?", "tell me about yourself", "tell me about Brasch",
		"what happened to you on Cyberstan?",
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

func TestSanitize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		bad  []string
	}{
		{"tool call", "Hulk. search_units(Hulk) Rear vent.", []string{"search_units("}},
		{"call id", "function_call call_id abc123 Rear vent.", []string{"function_call", "call_id", "abc123"}},
		{"json args", `Rear vent. {"query":"Hulk","k":3}`, []string{`"query"`}},
		{"narration", "Calling search_units now\nRear vent.", []string{"Calling search_units"}},
		{"planet fn", "planet_status(Mintoria) says held.", []string{"planet_status("}},
	}
	for _, c := range cases {
		got := Postprocess(c.in)
		for _, b := range c.bad {
			if strings.Contains(got, b) {
				t.Fatalf("%s: leak %q in %q", c.name, b, got)
			}
		}
		if !strings.HasSuffix(got, "Over.") {
			t.Fatalf("%s: missing sign-off: %q", c.name, got)
		}
	}
	dup := "Rear vent. [source: mongo] More. [source: wiki/Hulk]"
	got := Postprocess(dup)
	if c := strings.Count(got, "[source:"); c != 0 {
		t.Fatalf("raw source blobs must collapse, got %q", got)
	}
	if c := strings.Count(got, "Source:"); c != 1 {
		t.Fatalf("exactly one human Source line wanted, got %q", got)
	}
	if !strings.Contains(got, "Rear vent.") {
		t.Fatalf("intel body must survive sanitize: %q", got)
	}
	clean := "Hulk. Rear vent AMR."
	if got := Sanitize(clean); got != clean {
		t.Fatalf("clean text must pass through: %q", got)
	}
}
