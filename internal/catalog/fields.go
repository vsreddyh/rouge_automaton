package catalog

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// curatedFields never come from the catalog: they are the seed job's work
// (weak points, counters, aliases, role, params). A sync reports a
// disagreement instead of overwriting one of these.
var curatedFields = map[string]bool{
	"weak_points": true, "counters": true, "aliases": true,
	"role": true, "params": true,
}

// CuratedField reports whether a field belongs to the curated seed set.
func CuratedField(name string) bool { return curatedFields[name] }

// catalogFields are the fields the catalog owns. Curated seed fields are
// never in this map.
func catalogFields(it Item, info contextInfo, version string, now time.Time) bson.M {
	set := bson.M{
		"catalog_id":         it.ID,
		"catalog_fetched_at": now,
		"upcoming":           it.Upcoming,
		"live":               !it.Upcoming,
	}
	if version != "" {
		set["catalog_version"] = version
	}
	if it.Wiki.Title != "" {
		set["wiki_title"] = it.Wiki.Title
	}
	if it.Wiki.URL != "" {
		set["wiki_url"] = it.Wiki.URL
		// The wiki page is the provenance the seeds never had.
		set["source_url"] = it.Wiki.URL
	}
	if len(it.Wiki.Flags) > 0 {
		set["wiki_flags"] = it.Wiki.Flags
	}
	if it.Category != "" {
		set["category"] = it.Category
	}
	if it.Subcategory != "" {
		set["subcategory"] = it.Subcategory
	}
	if it.Kind != "" {
		set["kind"] = it.Kind
	}
	if it.Description != "" {
		set["catalog_description"] = it.Description
	}
	if len(it.TraitIDs) > 0 {
		set["trait_ids"] = it.TraitIDs
		names := make([]string, 0, len(it.TraitIDs))
		for _, id := range it.TraitIDs {
			if n, ok := info.traitNames[id]; ok && n != "" {
				names = append(names, n)
				continue
			}
			names = append(names, id)
		}
		set["traits"] = names
	}
	if len(it.Firearm) > 0 {
		set["firearm"] = toBsonM(it.Firearm)
	}
	if len(it.Throwable) > 0 {
		set["throwable"] = toBsonM(it.Throwable)
	}
	if len(it.Melee) > 0 {
		set["melee"] = toBsonM(it.Melee)
	}
	if len(it.Attacks) > 0 {
		set["attacks"] = bsonList(it.Attacks)
	}
	if len(it.StatsRaw) > 0 {
		set["stats_raw"] = toBsonM(it.StatsRaw)
	}
	if len(it.Source) > 0 {
		set["catalog_source"] = toBsonM(it.Source)
	}
	if label := sourceLabel(it); label != "" {
		set["warbond"] = label
	}
	// Stratagems.
	if it.PermitType != "" {
		set["permit_type"] = it.PermitType
	}
	if it.Availability != "" {
		set["availability"] = it.Availability
	}
	if len(it.Code) > 0 {
		set["code"] = it.Code
	}
	if it.CooldownS != nil {
		set["cooldown_s"] = it.CooldownS
	}
	if it.CallInTimeS != nil {
		set["call_in_time_s"] = it.CallInTimeS
	}
	if it.CallInTimeUpgradedS != nil {
		set["call_in_time_upgraded_s"] = it.CallInTimeUpgradedS
	}
	if it.Uses != nil {
		set["uses"] = it.Uses
	}
	if it.UnlockLevel != nil {
		set["unlock_level"] = it.UnlockLevel
	}
	if len(it.ShipModules) > 0 {
		set["ship_modules"] = bsonList(it.ShipModules)
	}
	if len(it.SupportWeapon) > 0 {
		set["support_weapon"] = toBsonM(it.SupportWeapon)
	}
	if len(it.Backpack) > 0 {
		set["backpack"] = toBsonM(it.Backpack)
	}
	if len(it.CooldownVariants) > 0 {
		vals := make([]any, 0, len(it.CooldownVariants))
		for _, v := range it.CooldownVariants {
			vals = append(vals, v)
		}
		set["cooldown_variants"] = bsonList(vals)
	}
	// Armor.
	if it.Weight != "" {
		set["weight"] = it.Weight
	}
	if it.ArmorRating != nil {
		set["armor_rating"] = it.ArmorRating
	}
	if it.Speed != nil {
		set["speed"] = it.Speed
	}
	if it.StaminaRegen != nil {
		set["stamina_regen"] = it.StaminaRegen
	}
	if it.PassiveID != "" {
		set["passive_id"] = it.PassiveID
		if n, ok := info.passiveNames[it.PassiveID]; ok && n != "" {
			set["passive"] = n
		}
		if fx := info.passiveFx[it.PassiveID]; len(fx) > 0 {
			set["passive_effects"] = fx
		}
	}
	if len(it.SetIDs) > 0 {
		set["set_ids"] = it.SetIDs
	}
	if it.ArmorID != "" {
		set["armor_id"] = it.ArmorID
	}
	if it.HelmetID != "" {
		set["helmet_id"] = it.HelmetID
	}
	if it.CapeID != "" {
		set["cape_id"] = it.CapeID
	}
	// Boosters.
	if it.Effect != "" {
		set["effect"] = it.Effect
	}
	if len(it.Effects) > 0 {
		set["effects"] = it.Effects
	}
	return set
}

// sourceLabel renders where an item comes from ("Ironclad Democracy P1").
func sourceLabel(it Item) string {
	if len(it.Source) == 0 {
		return ""
	}
	if label, ok := it.Source["label"].(string); ok && label != "" {
		return label
	}
	if typ, ok := it.Source["type"].(string); ok && typ != "" {
		return typ
	}
	return ""
}

// Mismatch is one disagreement between a curated seed field and the catalog.
type Mismatch struct {
	Field   string `bson:"field"`
	Curated string `bson:"curated"`
	Catalog string `bson:"catalog"`
}

var numberRe = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)

// diffCurated reports where the curated seed and the catalog disagree. The
// curated value wins; the difference is recorded, never applied.
func diffCurated(doc bson.M, it Item) []Mismatch {
	out := []Mismatch{}
	params, ok := paramBag(doc)
	if !ok {
		return out
	}
	// Damage per second: the catalog carries both firing modes, a seed
	// usually carries one.
	if len(it.Firearm) > 0 {
		if dps, ok := it.Firearm["dps"].([]any); ok && len(dps) > 0 {
			curated := toString(params["dps"])
			have := numbersIn(curated)
			missing := []string{}
			for _, d := range dps {
				n, ok := toFloat(d)
				if !ok {
					continue
				}
				if !containsNumber(have, n) {
					missing = append(missing, trimNumber(n))
				}
			}
			if len(missing) > 0 {
				out = append(out, Mismatch{
					Field:   "params.dps",
					Curated: curated,
					Catalog: strings.Join(missing, ", "),
				})
			}
		}
	}
	// Armor numbers.
	for _, pair := range []struct{ key, field string }{
		{"armor", "armor_rating"},
		{"speed", "speed"},
		{"stam_regen", "stamina_regen"},
	} {
		curated := toString(params[pair.key])
		if curated == "" {
			continue
		}
		var catalogVal any
		switch pair.field {
		case "armor_rating":
			catalogVal = it.ArmorRating
		case "speed":
			catalogVal = it.Speed
		case "stamina_regen":
			catalogVal = it.StaminaRegen
		}
		if catalogVal == nil {
			continue
		}
		catNum, ok := toFloat(catalogVal)
		if !ok {
			continue
		}
		curNum, ok := toFloat(curated)
		if !ok {
			continue
		}
		if curNum != catNum {
			out = append(out, Mismatch{
				Field:   "params." + pair.key,
				Curated: curated,
				Catalog: trimNumber(catNum),
			})
		}
	}
	return out
}

// paramBag reads a doc's nested params document in any writer shape: the
// same doc can come from a seed, from a crawl or from a catalog sync.
func paramBag(doc bson.M) (map[string]any, bool) {
	switch t := doc["params"].(type) {
	case bson.M:
		return map[string]any(t), true
	case map[string]any:
		return t, true
	case bson.D:
		out := map[string]any{}
		for _, e := range t {
			out[e.Key] = e.Value
		}
		return out, true
	}
	return nil, false
}

// numbersIn pulls the numbers out of a prose value.
func numbersIn(s string) []float64 {
	var out []float64
	for _, m := range numberRe.FindAllString(s, -1) {
		if n, err := strconv.ParseFloat(strings.ReplaceAll(m, ",", ""), 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func containsNumber(list []float64, want float64) bool {
	for _, n := range list {
		if n == want {
			return true
		}
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(t), ",", ""), 64)
		return f, err == nil
	}
	return 0, false
}

func trimNumber(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// paramsFromStats turns the catalog's raw stat table into a params bag, so
// a doc the catalog creates answers with the same shape as a seeded one.
func paramsFromStats(stats map[string]any) bson.M {
	out := bson.M{}
	for k, v := range stats {
		key := paramKey(k)
		if key == "" {
			continue
		}
		out[key] = toString(v)
	}
	return out
}

// paramKey folds a stat label into a params key ("Standard Damage" ->
// "standard_damage").
func paramKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	key := strings.Trim(b.String(), "_")
	for strings.Contains(key, "__") {
		key = strings.ReplaceAll(key, "__", "_")
	}
	return key
}

// toString renders a BSON value as one line of text.
func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return trimNumber(t)
	case float32:
		return trimNumber(float64(t))
	case int:
		return strconv.Itoa(t)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return fmt.Sprint(v)
}

// stringList reads a list field in any writer shape.
func stringList(doc bson.M, key string) []string {
	switch t := doc[key].(type) {
	case bson.A:
		var out []string
		for _, e := range t {
			if s := toString(e); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	case []any:
		var out []string
		for _, e := range t {
			if s := toString(e); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{t}
	}
	return nil
}

// toBsonM converts a decoded JSON map into a BSON-safe document, so the
// catalog's nested objects land as documents instead of opaque blobs.
func toBsonM(m map[string]any) bson.M {
	out := bson.M{}
	for k, v := range m {
		out[k] = bsonValue(v)
	}
	return out
}

// bsonList converts a decoded JSON list into a BSON-safe array, so nested
// documents land as documents instead of opaque blobs.
func bsonList(items []any) bson.A {
	arr := make(bson.A, 0, len(items))
	for _, item := range items {
		arr = append(arr, bsonValue(item))
	}
	return arr
}

func bsonValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return toBsonM(t)
	case []any:
		arr := make(bson.A, 0, len(t))
		for _, item := range t {
			arr = append(arr, bsonValue(item))
		}
		return arr
	default:
		return v
	}
}
