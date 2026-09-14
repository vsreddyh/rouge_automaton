package wikiparse

import "testing"

func TestInfobox(t *testing.T) {
	wt := `{{Infobox Enemy
| size = 2
| health = 1,800
| min_difficulty = {{Difficulty|4}}
| damage_type = {{Damage|Ballistic}} <br> {{Damage|Explosion}}
}}`
	box := Infobox(wt)
	if box["size"] != "2" || box["health"] != "1,800" {
		t.Fatalf("box = %v", box)
	}
	if len(DamageTypes(box["damage_type"])) != 2 {
		t.Fatalf("damage types = %v", DamageTypes(box["damage_type"]))
	}
}

func TestTacticsWithSubsection(t *testing.T) {
	wt := `Lead text.
== Tactical Information ==
=== Hulks ===
* Heavily armed and armored, demand special attention
* Flank to the rear vent
== Trivia ==
* Not a tactic`
	got := Tactics(wt, 8)
	if len(got) != 2 {
		t.Fatalf("tactics must survive a subsection heading: %v", got)
	}
}

func TestAnatomy(t *testing.T) {
	wt := `== Anatomy ==
{{Anatomy Table|
  {{Anatomy Row
    | part_name = Main
    | health = 1,800
    | av = 4
    | fatal = Yes
  }}
  {{Anatomy Row
    | part_name = Head
    | health = 250
    | av = 4
    | fatal = Yes
  }}
}}`
	rows := Anatomy(wt, 12)
	if len(rows) != 2 || rows[0].Part != "Main" || rows[1].Health != "250" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestSectionListNoneVsMissing(t *testing.T) {
	none := "== Environmental Conditions ==\nThis biome has no environmental conditions.\n"
	items, ok := SectionList(none, "Environmental Conditions", 12)
	if len(items) != 0 || !ok {
		t.Fatalf("verified-none: items=%v ok=%v", items, ok)
	}
	if _, ok := SectionList(none, "Weather", 12); ok {
		t.Fatal("absent section must report exists=false")
	}
	with := "== Environmental Conditions ==\n{{Environmental Conditions|Intense Heat|Sandstorms}}\n"
	items, ok = SectionList(with, "Environmental Conditions", 12)
	if !ok || len(items) != 2 {
		t.Fatalf("template items = %v ok=%v", items, ok)
	}
}

func TestDifficultyAndSize(t *testing.T) {
	if got := DifficultyName("{{Difficulty|4}}"); got != "Challenging (4)" {
		t.Fatalf("difficulty = %q", got)
	}
	if got := SizeName("2"); got != "Large" {
		t.Fatalf("size = %q", got)
	}
	if got := Health("1,800"); got != 1800 {
		t.Fatalf("health = %d", got)
	}
}
