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

// quoteFold turns every flavor of quote into one ASCII quote.
var quoteFold = strings.NewReplacer("’", "'", "‘", "'", "“", "\"", "”", "\"")

// tokenize splits a query into words: whitespace-separated, with
// punctuation trimmed off the edges. Slashes stay inside a word so
// designations ("AR-23C") and subpage titles ("Blitz Search And
// Destroy/Automaton") survive as one token.
func tokenize(q string) []string {
	fields := strings.Fields(q)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		// The quotes are folded before the trim so a quoted word keeps
		// its letters.
		f = quoteFold.Replace(f)
		f = strings.Trim(f, ".,;:!?\"'()[]{}")
		f = strings.ReplaceAll(f, "|", "")
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

// dropGameName removes the game's own name: the fallback wiki is the
// Helldivers 2 wiki, so every page it holds is about this game and its
// name narrows nothing. The "2" of "Helldivers 2" goes with it.
func dropGameName(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		low := strings.ToLower(t)
		nextIsTwo := i+1 < len(tokens) && tokens[i+1] == "2"
		switch {
		case low == "helldivers" || low == "helldivers2":
			if nextIsTwo {
				i++ // "Helldivers 2" is one name: drop the "2" with it
			}
		case low == "helldiver" && nextIsTwo:
			// "Helldiver 2" is the same name, misspelled.
			i++
		default:
			out = append(out, t)
		}
	}
	return out
}

// contentKeywords drops the framing words: the question scaffolding.
func contentKeywords(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if isFraming(t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// isFraming reports whether a word carries no page signal.
func isFraming(t string) bool {
	return framingWords[strings.ToLower(t)]
}

// ngrams returns the contiguous word runs of tokens, longest first.
// Within a length a run whose first word is a name comes first — the
// model capitalizes entity names ("Tundra", "Hulk Bruiser") and leaves
// common words ("weapons", "biome") alone — and among equals the later
// mention comes first: a question names its subject last ("how do I
// kill Hulk Bruiser").
//
// A run that starts or ends with a framing word is dropped: scaffolding
// is not part of a page name. A framing word inside a run stays, because
// a page may spell its name with one ("Blitz Search And Destroy").
func ngrams(tokens []string) []string {
	out := make([]string, 0, len(tokens)*maxTitleWords)
	for n := maxTitleWords; n >= 1; n-- {
		var named, common []string
		for i := len(tokens) - n; i >= 0; i-- {
			if isFraming(tokens[i]) || isFraming(tokens[i+n-1]) {
				continue
			}
			run := strings.Join(tokens[i:i+n], " ")
			if isNamed(tokens[i]) {
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
	tokens := dropGameName(tokenize(raw))
	// Titles keep the question's own wording: a page may spell its name
	// with a framing word inside it ("Blitz Search And Destroy"), so
	// the runs come from the tokens as written.
	titles = dedupeFold(ngrams(tokens))
	if len(titles) > maxTitleCandidates {
		titles = titles[:maxTitleCandidates]
	}
	// The raw question goes first: when the model passes a page name
	// ("Arc Cannon") the wiki answers it directly. Then the de-framed
	// keywords, then shorter runs of them — MediaWiki ANDs every term,
	// so a query one term too long misses a page that has the rest.
	kw := contentKeywords(tokens)
	if len(kw) == 0 {
		// Nothing but question scaffolding: there is no topic to look
		// up, so no request is worth making.
		return nil, nil
	}
	searches = []string{raw}
	searches = append(searches, strings.Join(kw, " "))
	searches = append(searches, ngrams(kw)...)
	// The wiki matches whatever the case and the trailing punctuation,
	// so two spellings of one query are one request.
	searches = dedupeBy(searches, searchKey)
	if len(searches) > maxSearches {
		searches = searches[:maxSearches]
	}
	return titles, searches
}

// searchKey is the dedupe key of a search string.
func searchKey(s string) string {
	return strings.ToLower(strings.Trim(s, ".,;:!?\"'()[]{}"))
}

// dedupeBy keeps the first occurrence of each string and drops the
// rest, comparing the strings by key.
func dedupeBy(in []string, key func(string) string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		k := key(s)
		if s == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	return out
}

// dedupeFold keeps the first occurrence of each string and drops the
// rest, comparing case-insensitively.
func dedupeFold(in []string) []string {
	return dedupeBy(in, strings.ToLower)
}
