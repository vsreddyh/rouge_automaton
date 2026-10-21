package wiki

import (
	"strings"
	"unicode"
)

// Query shaping.
//
// The model hands wiki_search a natural-language question ("best loadout
// vs Automatons"), but MediaWiki ANDs every term of a search and knows
// nothing about question framing: the raw question scores zero hits while
// the wiki covers the entity. Shaping turns the raw question into a
// ladder of lookups — exact page titles first (the wiki's own index of
// "the page about this thing"), then free-text searches, each shorter
// than the last.

const (
	// maxTitleWords is the longest entity name resolved as an exact
	// page title ("Jet Brigade Hulk Bruiser").
	maxTitleWords = 4
	// maxTitleCandidates caps one batched title lookup; MediaWiki
	// rejects more than 50 titles in a single request.
	maxTitleCandidates = 40
	// maxSearches caps the free-text retries, so one hopeless query
	// cannot turn a single tool call into a burst of wiki requests.
	maxSearches = 5
)

// framingWords carry no page signal: question scaffolding ("what is",
// "how do I") and comparison glue ("vs"). MediaWiki requires every term
// to match, so one of them can zero out an answerable page.
var framingWords = map[string]bool{
	// question scaffolding
	"a": true, "about": true, "all": true, "am": true, "an": true,
	"and": true, "any": true, "are": true, "as": true, "at": true,
	"be": true, "been": true, "being": true, "best": true, "better": true,
	"but": true, "by": true, "can": true, "could": true, "did": true,
	"do": true, "does": true, "doing": true, "done": true, "for": true,
	"from": true, "get": true, "give": true, "go": true, "good": true,
	"great": true, "has": true, "have": true, "how": true, "i": true,
	"if": true, "in": true, "into": true, "is": true, "it": true,
	"its": true, "just": true, "know": true, "like": true, "look": true,
	"make": true, "many": true, "me": true, "more": true, "most": true,
	"much": true, "my": true, "need": true, "of": true, "on": true,
	"or": true, "our": true, "out": true, "over": true, "please": true,
	"same": true, "should": true, "show": true, "so": true, "some": true,
	"such": true, "tell": true, "than": true, "that": true, "the": true,
	"their": true, "them": true, "then": true, "there": true,
	"these": true, "they": true, "this": true, "those": true, "to": true,
	"too": true, "up": true, "us": true, "use": true, "very": true,
	"want": true, "was": true, "we": true, "were": true, "what": true,
	"whats": true, "when": true, "where": true, "which": true,
	"while": true, "who": true, "whom": true, "whose": true, "why": true,
	"will": true, "with": true, "would": true, "you": true, "your": true,

	// comparison glue
	"against": true, "between": true, "compared": true, "compares": true,
	"comparing": true, "comparison": true, "diff": true, "difference": true,
	"differences": true, "vs": true, "versus": true,
}

// normalizeQuery trims a query and collapses its whitespace.
func normalizeQuery(q string) string {
	return strings.TrimSpace(spaces.ReplaceAllString(q, " "))
}

// tokenize splits a query into words: whitespace-separated, with
// punctuation trimmed off the edges. Slashes stay inside a word so
// designations ("AR-23C") and subpage titles ("Blitz Search And
// Destroy/Automaton") survive as one token.
func tokenize(q string) []string {
	fields := strings.Fields(q)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.Trim(f, ".,;:!?\"'()[]{}")
		f = strings.ReplaceAll(f, "|", "")
		// A typed apostrophe is an apostrophe.
		f = strings.ReplaceAll(f, "’", "'")
		// Contractions carry no page signal: "what's" is the question
		// word "what", "don't" is noise.
		if i := strings.Index(f, "'"); i >= 0 {
			if strings.EqualFold(f[i:], "'s") {
				f = f[:i]
			} else {
				f = ""
			}
		}
		if f == "" {
			continue
		}
		out = append(out, f)
	}
	return out
}

// contentKeywords drops the framing words and the game's own name: the
// fallback wiki is the Helldivers 2 wiki, so every page it holds is
// about this game and its name narrows nothing.
func contentKeywords(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		low := strings.ToLower(t)
		if low == "helldivers" {
			if i+1 < len(tokens) && strings.EqualFold(tokens[i+1], "2") {
				i++ // "Helldivers 2" is one name: drop the "2" with it
			}
			continue
		}
		if framingWords[low] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// ngrams returns the contiguous word runs of kw, longest first. Within a
// length a run whose first word is a name comes first — the model
// capitalizes entity names ("Tundra", "Hulk Bruiser") and leaves common
// words ("weapons", "biome") alone — and among equals the later mention
// comes first: a question names its subject last ("how do I kill Hulk
// Bruiser").
func ngrams(kw []string) []string {
	out := make([]string, 0, len(kw)*maxTitleWords)
	for n := maxTitleWords; n >= 1; n-- {
		var named, common []string
		for i := len(kw) - n; i >= 0; i-- {
			run := strings.Join(kw[i:i+n], " ")
			if isNamed(kw[i]) {
				named = append(named, run)
				continue
			}
			common = append(common, run)
		}
		out = append(out, named...)
		out = append(out, common...)
	}
	return out
}

// isNamed reports whether a word starts with an uppercase letter.
func isNamed(t string) bool {
	r := []rune(t)
	return len(r) > 0 && unicode.IsUpper(r[0])
}

// shape splits a query into the exact page titles worth trying and the
// free-text searches to fall back on.
func shape(raw string) (titles, searches []string) {
	raw = normalizeQuery(raw)
	if raw == "" {
		return nil, nil
	}
	kw := contentKeywords(tokenize(raw))
	// Titles resolve case-insensitively, so two spellings of one name
	// would spend two slots of the title batch.
	titles = dedupeFold(ngrams(kw))
	if len(titles) > maxTitleCandidates {
		titles = titles[:maxTitleCandidates]
	}
	// The raw question goes first: when the model passes a page name
	// ("Arc Cannon") the wiki answers it directly. Then the de-framed
	// keywords, then shorter runs of them — MediaWiki ANDs every term,
	// so a query one term too long misses a page that has the rest.
	searches = []string{raw}
	if full := strings.Join(kw, " "); full != "" {
		searches = append(searches, full)
	}
	searches = append(searches, ngrams(kw)...)
	searches = dedupe(searches)
	if len(searches) > maxSearches {
		searches = searches[:maxSearches]
	}
	return titles, searches
}

// dedupe keeps the first occurrence of each string and drops the rest,
// preserving order.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// dedupeFold is dedupe with a case-insensitive key: MediaWiki resolves
// titles that differ only in case, so keeping both would waste a slot
// of the title batch.
func dedupeFold(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		key := strings.ToLower(s)
		if s == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	return out
}
