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
	wantTitles := []string{"loadout vs Automatons", "Automatons", "loadout"}
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
	want := []string{"stratagem against Terminids", "Terminids", "stratagem"}
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

func TestShapeKeepsFunctionWordInsideName(t *testing.T) {
	// A page may spell its name with a framing word inside it.
	titles, _ := shape("Blitz Search And Destroy")
	want := []string{
		"Blitz Search And Destroy",
		"Search And Destroy",
		"Blitz Search",
		"Destroy",
		"Search",
		"Blitz",
	}
	if !equalStrings(titles, want) {
		t.Fatalf("titles = %q, want %q", titles, want)
	}
}

func TestShapeSubjectLast(t *testing.T) {
	// A question names its subject last ("stratagems vs Terminids"), so
	// the later run wins a tie between same-length candidates.
	titles, _ := shape("weapons for automatons")
	want := []string{"weapons for automatons", "automatons", "weapons"}
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
	if got := tokenize("“Hulk Bruiser”"); !equalStrings(got, []string{"Hulk", "Bruiser"}) {
		t.Fatalf("tokens = %q, want [Hulk Bruiser]", got)
	}
}

func TestTokenizeKeepsDesignations(t *testing.T) {
	got := tokenize("AR-23C Arc Cannon")
	want := []string{"AR-23C", "Arc", "Cannon"}
	if !equalStrings(got, want) {
		t.Fatalf("tokens = %q, want %q", got, want)
	}
}

func TestContentKeywords(t *testing.T) {
	got := contentKeywords(tokenize("best loadout vs Automatons"))
	want := []string{"loadout", "Automatons"}
	if !equalStrings(got, want) {
		t.Fatalf("keywords = %q, want %q", got, want)
	}
}

func TestDropGameName(t *testing.T) {
	got := dropGameName(tokenize("best loadout vs Automatons Helldivers 2"))
	want := []string{"best", "loadout", "vs", "Automatons"}
	if !equalStrings(got, want) {
		t.Fatalf("tokens = %q, want %q", got, want)
	}
	if strings.Contains(strings.Join(got, " "), "helldivers") {
		t.Fatal("the game name must not survive as a token")
	}
}

func TestDropGameNameVariants(t *testing.T) {
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{"Helldivers 2 Automatons", []string{"Automatons"}},
		{"Helldiver 2 Automatons", []string{"Automatons"}},
		{"Helldivers2 Automatons", []string{"Automatons"}},
		// On its own the singular is the player, not the game.
		{"Helldiver Automatons", []string{"Helldiver", "Automatons"}},
	} {
		got := dropGameName(tokenize(tc.q))
		if !equalStrings(got, tc.want) {
			t.Fatalf("dropGameName(%q) = %q, want %q", tc.q, got, tc.want)
		}
	}
}

func TestShapeDedupeSpelling(t *testing.T) {
	// The wiki ignores case and trailing punctuation, so two spellings
	// of one query are one request.
	_, searches := shape("Automatons?")
	want := []string{"Automatons?"}
	if !equalStrings(searches, want) {
		t.Fatalf("searches = %q, want %q", searches, want)
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
