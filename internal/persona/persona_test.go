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
	// The serial is backstory, not a signature: it belongs to SOUL.md only,
	// so everyday prompts never prime the model to paste it into a greeting.
	if strings.Contains(got, "[REDACTED]") {
		t.Fatal("intel prompt must not carry the serial")
	}
	if n := strings.Count(got, "Helldiver"); n > 1 {
		t.Fatalf("Helldiver must not be repeated in everyday prompts, got %d", n)
	}
	// Issue #44: brevity rules dropped — no terse/short-burst phrasing and
	// no word cap on replies anywhere in the prompt layers.
	for _, banned := range []string{"terse", "short bursts", "120 words", "<120"} {
		if strings.Contains(strings.ToLower(got), banned) {
			t.Fatalf("intel prompt must not carry brevity rule %q", banned)
		}
	}
	// Issue #44: the old never-clarify rule is replaced by the escape
	// hatch — one short question only when genuinely ambiguous and no
	// tool resolves it, with zero-argument questions answered directly.
	if strings.Contains(got, "Never ask the user to clarify") {
		t.Fatal("prompts must drop the never-clarify rule (issue #44)")
	}
	if !strings.Contains(strings.ToLower(got), "clarifying question") {
		t.Fatal("prompts must keep the clarification escape hatch")
	}
	if !strings.Contains(got, "as fully as the question needs") {
		t.Fatal("prompts must allow answers as full as the question needs")
	}
	if !strings.Contains(got, "never refuse") {
		t.Fatal("intel prompt must keep the equal-faction never-refuse rule")
	}
	// The prompt layer itself stays concise (<120 words) so it never
	// inherits backstory weight — this is prompt hygiene, not a reply cap.
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
		keep []string
	}{
		{"tool call", "Hulk. get_unit(Hulk) Rear vent.", []string{"get_unit("}, []string{"Rear vent"}},
		{"bare tool", "Intel says planet_status is held.", []string{"planet_status"}, []string{"is held"}},
		{"call chain", "function_call call_id abc123 Rear vent.", []string{"function_call", "call_id", "abc123"}, []string{"Rear vent"}},
		{"bare marker", "function_call Rear vent.", []string{"function_call"}, []string{"Rear vent"}},
		{"prose narration kept", "Searching the archives, Helldiver, rear vent exposed.", []string{}, []string{"rear vent exposed"}},
		{"call id", "call_id: XYZ-9 Rear vent.", []string{"call_id", "XYZ-9"}, []string{"Rear vent"}},
		{"json args", `Rear vent. {"query":"Hulk","k":3}`, []string{`"query"`, "{"}, []string{"Rear vent"}},
		{"json new keys", `Held. {"planet":"X","top_k":2}`, []string{"top_k", "{"}, []string{"Held"}},
		{"prose braces kept", `Vent {front} exposed.`, []string{}, []string{"{front}"}},
		{"narration", "Calling get_unit now\nRear vent.", []string{"Calling get_unit", "Calling now"}, []string{"Rear vent"}},
		{"narration period", "Calling get_unit now.\nRear vent.", []string{"Calling now"}, []string{"Rear vent"}},
		{"narration sameline kept", "Calling get_unit now Rear vent.", []string{}, []string{"Rear vent"}},
		{"call bare id", "call_id Rear vent.", []string{"call_id Rear"}, []string{"Rear vent"}},
		{"function direct id", "function_call abc123 Rear vent.", []string{"function_call", "abc123"}, []string{"Rear vent"}},
		{"planet fn", "planet_status(Mintoria) says held.", []string{"planet_status("}, []string{"says held"}},
		{"list units", "Roster. list_units(automatons) Hulk.", []string{"list_units("}, []string{"Hulk"}},
		{"get gear", "Boom. get_gear(Railcannon) fires.", []string{"get_gear("}, []string{"fires"}},
		{"get place", "Cold. get_place(Tundra) ice.", []string{"get_place("}, []string{"ice"}},
		// Anchored narrateRe: narration verbs before non-tools are prose,
		// not leaks — only the exact tool set strips.
		{"narration non-tool kept", "Calling get_ready now\nRear vent.", []string{}, []string{"Calling get_ready", "Rear vent"}},
	}
	for _, c := range cases {
		got := Postprocess(c.in)
		for _, b := range c.bad {
			if strings.Contains(got, b) {
				t.Fatalf("%s: leak %q in %q", c.name, b, got)
			}
		}
		for _, k := range c.keep {
			if !strings.Contains(got, k) {
				t.Fatalf("%s: body lost %q in %q", c.name, k, got)
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
	// A pre-existing human Source line must not duplicate.
	if got := Postprocess("Rear vent. Source: mongo [source: mongo]"); strings.Count(got, "Source:") != 1 {
		t.Fatalf("duplicate Source lines: %q", got)
	}
	// Empty input signs off without a leading space.
	if got := Postprocess("   "); got != "Over." {
		t.Fatalf("empty reply must be bare sign-off: %q", got)
	}
	// Uppercase source blobs collapse the same way.
	if got := Postprocess("Rear vent. [SOURCE: mongo]"); strings.Contains(got, "[SOURCE:") || strings.Count(got, "Source:") != 1 {
		t.Fatalf("uppercase source must collapse: %q", got)
	}
	// Quoted/semicolon sources fold against bare ones.
	if got := Postprocess(`Rear vent. [source: "mongo"]`); strings.Count(got, "Source:") != 1 {
		t.Fatalf("quoted source must fold: %q", got)
	}
	// Punctuated blobs normalize to one period, never two.
	if got := Postprocess(`Rear vent. [source: mongo.]`); strings.Contains(got, "mongo..") {
		t.Fatalf("double period: %q", got)
	}
	// Nested JSON clears fully; mongo must not prefix-match mongodb.
	if got := Postprocess(`Vent. {"outer": {"inner": 1}}`); strings.Contains(got, "{") {
		t.Fatalf("nested JSON must clear: %q", got)
	}
	if got := Postprocess("Rear vent. Source: mongodb [source: mongo]"); !strings.Contains(got, "Source: mongodb") || !strings.Contains(got, "Source: mongo") {
		t.Fatalf("mongo must not shadow mongodb: %q", got)
	}
	// Multi-word sources fold as one phrase, not one token.
	if got := Postprocess("Rear vent. Source: Super Earth archives [source: Super Earth archives]"); strings.Count(got, "Source:") != 1 {
		t.Fatalf("multi-word source must not duplicate: %q", got)
	}
	// A trailing sourceless Source line gains its period before sign-off.
	if got := Postprocess("Rear vent. Source: mongo"); got != "Rear vent. Source: mongo. Over." {
		t.Fatalf("trailing source needs its period: %q", got)
	}
	// Middle Source lines gain periods too when a second blob appends.
	if got := Postprocess("Rear vent. Source: mongodb [source: mongo]"); got != "Rear vent. Source: mongodb.\nSource: mongo. Over." {
		t.Fatalf("middle source needs its period: %q", got)
	}
	clean := "Hulk. Rear vent AMR."
	if got := Sanitize(clean); got != clean {
		t.Fatalf("clean text must pass through: %q", got)
	}
}
