package wiki

import (
	"strings"
	"testing"
)

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestShapeAcceptanceQuery(t *testing.T) {
	// The convo-test query: raw natural language that scores zero hits
	// on the wiki. The shaped titles must reach the entity page, and
	// the searches must try the raw question, the de-framed keywords
	// and then the shorter runs of them.
	titles, searches := shape("best loadout vs Automatons Helldivers 2")
	wantTitles := []string{"loadout Automatons", "Automatons", "loadout"}
	if !equalStrings(titles, wantTitles) {
		t.Fatalf("titles = %q, want %q", titles, wantTitles)
	}
	wantSearches := []string{
		"best loadout vs Automatons Helldivers 2",
		"loadout Automatons",
		"Automatons",
		"loadout",
	}
	if !equalStrings(searches, wantSearches) {
		t.Fatalf("searches = %q, want %q", searches, wantSearches)
	}
}

func TestShapeDropsFraming(t *testing.T) {
	titles, _ := shape("what is the best stratagem against Terminids")
	want := []string{"stratagem Terminids", "Terminids", "stratagem"}
	if !equalStrings(titles, want) {
		t.Fatalf("titles = %q, want %q", titles, want)
	}
}

func TestShapeDropsGameName(t *testing.T) {
	// The wiki is the Helldivers 2 wiki: its name narrows nothing.
	titles, searches := shape("Automatons Helldivers 2")
	if !equalStrings(titles, []string{"Automatons"}) {
		t.Fatalf("titles = %q", titles)
	}
	if searches[0] != "Automatons Helldivers 2" {
		t.Fatalf("the raw question must stay the first search: %q", searches)
	}
}

func TestShapeKeepsEntityPhrase(t *testing.T) {
	titles, _ := shape("how do I kill Hulk Bruiser")
	want := []string{"kill Hulk Bruiser", "Hulk Bruiser", "kill Hulk", "Bruiser", "Hulk", "kill"}
	if !equalStrings(titles, want) {
		t.Fatalf("titles = %q, want %q", titles, want)
	}
}

func TestShapeSubjectLast(t *testing.T) {
	// A question names its subject last ("stratagems vs Terminids"), so
	// the later run wins a tie between same-length candidates.
	titles, _ := shape("weapons for automatons")
	want := []string{"weapons automatons", "automatons", "weapons"}
	if !equalStrings(titles, want) {
		t.Fatalf("titles = %q, want %q", titles, want)
	}
}

func TestShapeNamedBeforeCommon(t *testing.T) {
	// The model capitalizes entity names, so "Tundra" outranks the hub
	// word "biome" even though the hub word comes later in the query.
	titles, _ := shape("Tundra biome hazards")
	want := []string{
		"Tundra biome hazards",
		"Tundra biome",
		"biome hazards",
		"Tundra",
		"hazards",
		"biome",
	}
	if !equalStrings(titles, want) {
		t.Fatalf("titles = %q, want %q", titles, want)
	}
}

func TestShapeEmpty(t *testing.T) {
	titles, searches := shape("   ")
	if titles != nil || searches != nil {
		t.Fatalf("empty query must shape to nothing: %q %q", titles, searches)
	}
}

func TestShapeCapsSearches(t *testing.T) {
	_, searches := shape("what is the best loadout to use against the automatons in helldivers 2 today")
	if len(searches) > maxSearches {
		t.Fatalf("searches = %d, want at most %d", len(searches), maxSearches)
	}
}

func TestTokenize(t *testing.T) {
	got := tokenize("what's the best loadout, versus automatons?")
	want := []string{"what", "the", "best", "loadout", "versus", "automatons"}
	if !equalStrings(got, want) {
		t.Fatalf("tokens = %q, want %q", got, want)
	}
}

func TestTokenizeCurlyApostrophe(t *testing.T) {
	got := tokenize("what’s the best loadout")
	want := []string{"what", "the", "best", "loadout"}
	if !equalStrings(got, want) {
		t.Fatalf("tokens = %q, want %q", got, want)
	}
	// A quoted word keeps its letters: the quotes are folded first and
	// trimmed after.
	if got := tokenize("’hello’"); !equalStrings(got, []string{"hello"}) {
		t.Fatalf("tokens = %q, want [hello]", got)
	}
}

func TestDedupeFold(t *testing.T) {
	// Titles resolve case-insensitively, so both spellings must not
	// spend two slots of the title batch.
	got := dedupeFold([]string{"Automatons", "automatons", "Tundra", "tundra", "loadout"})
	want := []string{"Automatons", "Tundra", "loadout"}
	if !equalStrings(got, want) {
		t.Fatalf("dedupeFold = %q, want %q", got, want)
	}
}

func TestTokenizeKeepsDesignations(t *testing.T) {
	got := tokenize("AR-23C Arc Cannon")
	want := []string{"AR-23C", "Arc", "Cannon"}
	if !equalStrings(got, want) {
		t.Fatalf("tokens = %q, want %q", got, want)
	}
}

func TestContentKeywordsKeepsEntityCase(t *testing.T) {
	got := contentKeywords(tokenize("best loadout vs Automatons Helldivers 2"))
	want := []string{"loadout", "Automatons"}
	if !equalStrings(got, want) {
		t.Fatalf("keywords = %q, want %q", got, want)
	}
	if strings.Contains(strings.Join(got, " "), "helldivers") {
		t.Fatal("the game name must not survive as a keyword")
	}
}
