// Package wikiparse extracts structured fields from MediaWiki wikitext.
// Ports the parser half of scripts/crawl_wiki.py (infobox, anatomy, tactics,
// biome sections) so the Go watcher can refresh cached docs without Python.
package wikiparse

import (
	"regexp"
	"sort"
	"strings"
)

// Difficulty maps in-game difficulty levels to names.
var Difficulty = map[int]string{
	1: "Trivial", 2: "Easy", 3: "Medium", 4: "Challenging", 5: "Hard",
	6: "Extreme", 7: "Suicide Mission", 8: "Impossible", 9: "Helldive",
	10: "Super Helldive",
}

// SizeClass maps the infobox size integer to its class name.
var SizeClass = map[int]string{0: "Small", 1: "Medium", 2: "Large", 3: "Massive"}

var (
	infoboxRe    = regexp.MustCompile(`(?m)^\s*\|\s*([^|=<>!\[\]\n]{1,40}?)\s*=\s*(.+?)\s*$`)
	tagRe        = regexp.MustCompile(`<[^>]+>`)
	linkRe       = regexp.MustCompile(`\[\[([^|\]]+\|)?([^\]]+)\]\]`)
	boldRe       = regexp.MustCompile(`'''?`)
	diffRe       = regexp.MustCompile(`\{\{Difficulty\|(\d+)\}\}`)
	damageRe     = regexp.MustCompile(`\{\{Damage\|([^}|]+)`)
	condTplRe    = regexp.MustCompile(`\{\{(?:Environmental Conditions|Weather)\|([^}]+)\}\}`)
	anatomyRowRe = regexp.MustCompile(`\{\{Anatomy Row`)
	rowParamRe   = regexp.MustCompile(`\|\s*([\w ]+?)\s*=\s*([^\n|]+)`)
	numRe        = regexp.MustCompile(`\d+`)
	healthRe     = regexp.MustCompile(`[\d,]+`)
)

// cut truncates to n runes (multibyte-safe). Wiki text routinely contains
// multibyte runes (em-dashes, icons); byte-slicing would split them and
// emit invalid UTF-8, which BSON rejects on write — failing the whole
// Refresh. Every truncation in this package goes through cut.
func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Infobox returns first-seen `| key = value` pairs with markup stripped.
// Keys are lowercased and truncated at 40 chars; values are capped at 300.
func Infobox(wikitext string) map[string]string {
	out := map[string]string{}
	for _, m := range infoboxRe.FindAllStringSubmatch(wikitext, -1) {
		k := strings.ToLower(strings.TrimSpace(m[1]))
		v := strings.TrimSpace(tagRe.ReplaceAllString(m[2], ""))
		if k == "" || v == "" {
			continue
		}
		if _, seen := out[k]; !seen {
			out[k] = cut(v, 300)
		}
	}
	return out
}

// section returns the body of a level-2 heading, and whether it exists.
func section(wikitext, heading string) (string, bool) {
	re := regexp.MustCompile(`(?is)==\s*` + regexp.QuoteMeta(heading) + `\s*==(.*?)(?:\n==[^=]|\z)`)
	m := re.FindStringSubmatch(wikitext)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func cleanBullet(line string) string {
	line = strings.TrimSpace(strings.TrimLeft(line, "* "))
	line = linkRe.ReplaceAllString(line, "$2")
	line = boldRe.ReplaceAllString(line, "")
	return strings.TrimSpace(line)
}

// Tactics returns bullet lines under "Tactical Information", capped.
func Tactics(wikitext string, cap int) []string {
	body, ok := section(wikitext, "Tactical Information")
	if !ok {
		return nil
	}
	var out []string
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "=") || !strings.HasPrefix(t, "*") || len(t) <= 4 {
			continue
		}
		if c := cleanBullet(t); c != "" {
			out = append(out, cut(c, 220))
		}
		if len(out) >= cap {
			break
		}
	}
	return out
}

// AnatomyRow is one parsed part of a unit.
type AnatomyRow struct {
	Part   string
	Health string
	AV     string
	Fatal  string
}

// Anatomy parses {{Anatomy Row ...}} templates via brace matching.
func Anatomy(wikitext string, cap int) []AnatomyRow {
	var rows []AnatomyRow
	for _, loc := range anatomyRowRe.FindAllStringIndex(wikitext, -1) {
		depth, i := 0, loc[0]
		for i < len(wikitext) {
			switch {
			case strings.HasPrefix(wikitext[i:], "{{"):
				depth++
				i += 2
			case strings.HasPrefix(wikitext[i:], "}}"):
				depth--
				i += 2
				if depth == 0 {
					goto done
				}
			default:
				i++
			}
		}
	done:
		block := wikitext[loc[0]:i]
		params := map[string]string{}
		for _, m := range rowParamRe.FindAllStringSubmatch(block, -1) {
			k := strings.ToLower(strings.TrimSpace(m[1]))
			v := strings.TrimSpace(tagRe.ReplaceAllString(m[2], ""))
			if _, seen := params[k]; !seen && v != "" {
				params[k] = cut(v, 80)
			}
		}
		if params["part_name"] == "" {
			continue
		}
		rows = append(rows, AnatomyRow{
			Part: params["part_name"], Health: params["health"],
			AV: params["av"], Fatal: params["fatal"],
		})
		if len(rows) >= cap {
			break
		}
	}
	return rows
}

// SectionList returns bullet/template items under a heading plus whether the
// section exists (so callers can distinguish "none" from "missing").
func SectionList(wikitext, heading string, cap int) ([]string, bool) {
	body, ok := section(wikitext, heading)
	if !ok {
		return nil, false
	}
	var items []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			items = append(items, cut(s, 220))
		}
	}
	for _, m := range condTplRe.FindAllStringSubmatch(body, -1) {
		for _, part := range strings.Split(m[1], "|") {
			add(part)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "*") || strings.HasPrefix(t, "**") || len(t) <= 3 {
			continue
		}
		add(cleanBullet(t))
	}
	if len(items) > cap {
		items = items[:cap]
	}
	return items, true
}

// DifficultyName renders "{{Difficulty|4}}" as "Challenging (4)".
func DifficultyName(raw string) string {
	m := diffRe.FindStringSubmatch(raw)
	if m == nil {
		return ""
	}
	n := atoi(m[1])
	if name, ok := Difficulty[n]; ok {
		return name + " (" + itoa(n) + ")"
	}
	return ""
}

// SpawningDifficulty falls back to the Spawning section for min difficulty.
func SpawningDifficulty(wikitext string) string {
	if body, ok := section(wikitext, "Spawning"); ok {
		return DifficultyName(body)
	}
	return ""
}

// DamageTypes lists distinct {{Damage|Type}} values, sorted.
func DamageTypes(raw string) []string {
	seen := map[string]bool{}
	for _, m := range damageRe.FindAllStringSubmatch(raw, -1) {
		seen[strings.TrimSpace(m[1])] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SizeName maps an infobox size value to its class name.
func SizeName(raw string) string {
	m := numRe.FindString(raw)
	if m == "" {
		return ""
	}
	if name, ok := SizeClass[atoi(m)]; ok {
		return name
	}
	return ""
}

// Health parses a comma-grouped health value (e.g. "1,800").
func Health(raw string) int {
	m := healthRe.FindString(raw)
	if m == "" {
		return 0
	}
	return atoi(strings.ReplaceAll(m, ",", ""))
}

// ExtractFirstParagraph returns the first blank-line-separated block, capped.
func ExtractFirstParagraph(extract string, cap int) string {
	first := extract
	if i := strings.Index(extract, "\n\n"); i >= 0 {
		first = extract[:i]
	}
	return cut(first, cap)
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
