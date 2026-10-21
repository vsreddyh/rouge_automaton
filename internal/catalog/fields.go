package catalog

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"rouge_automaton/internal/docval"
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

// paramCheck pairs a curated params key with the catalog field it is
// compared against.
//
// Only the fields both sources carry are listed. The catalog has no weak
// points and no counters, so those curated fields are never at risk. The
// stratagem vocabulary differs too (the seed says "Objective" and
// "Offense" where the catalog says "mission" and "offensive"), so those
// are not compared: a flag there would be noise, not a disagreement.
type paramCheck struct {
	param string
	field string
}

// weaponChecks are the weapon numbers the catalog carries precisely: a
// seed rounds them ("13" for 13.25), and the difference is worth a flag.
var weaponChecks = []paramCheck{
	{"capacity", "capacity"},
	{"dps", "dps"},
	{"fire_rate", "fireRateRpm"},
	{"recoil", "recoil"},
	{"reload_time", "reloadTimeS"},
	{"spare_mags", "spareMagazines"},
	{"tac_reload_time", "tacticalReloadTimeS"},
}

// armorChecks are the armor numbers.
var armorChecks = []paramCheck{
	{"armor", "armor"},
	{"speed", "speed"},
	{"stam_regen", "stam_regen"},
}

// stratagemChecks are the stratagem numbers. The catalog's cooldown is the
// one it lists (it carries the variants beside it).
var stratagemChecks = []paramCheck{
	{"base_cooldown", "cooldown"},
	{"unlock_level", "unlock_level"},
}

// diffCurated reports where the curated seed and the catalog disagree. The
// curated value wins; the difference is recorded, never applied.
//
// What is compared: the numbers both sources carry (a weapon's dps,
// capacity, recoil, ...; an armor's rating; a stratagem's cooldown), the
// aliases, and a role the seed never wrote. What is not: the prose. The
// seed's role and the catalog's description are two ways of saying the
// same thing, and a wording difference is not a disagreement.
func diffCurated(doc bson.M, it Item) []Mismatch {
	out := []Mismatch{}
	// A name the catalog knows and the curated list does not.
	if m := diffAliases(doc, it); m != nil {
		out = append(out, *m)
	}
	// A role the seed never wrote: the catalog's description could fill
	// it in, but writing it is the curator's job.
	if strings.TrimSpace(docval.Value(doc["role"])) == "" && it.Description != "" {
		out = append(out, Mismatch{Field: "role", Curated: "", Catalog: it.Description})
	}
	params, ok := paramBag(doc)
	if !ok {
		return out
	}
	if len(it.Firearm) > 0 {
		out = append(out, diffNumbers(params, weaponChecks, it.Firearm)...)
	}
	out = append(out, diffNumbers(params, armorChecks, armorValues(it))...)
	out = append(out, diffNumbers(params, stratagemChecks, stratagemValues(it))...)
	return out
}

// armorValues are the armor numbers the catalog carries.
func armorValues(it Item) map[string]any {
	return map[string]any{
		"armor":      it.ArmorRating,
		"speed":      it.Speed,
		"stam_regen": it.StaminaRegen,
	}
}

// stratagemValues are the stratagem numbers the catalog carries.
func stratagemValues(it Item) map[string]any {
	return map[string]any{
		"cooldown":     it.CooldownS,
		"unlock_level": it.UnlockLevel,
	}
}

// diffNumbers compares the curated params against the catalog's numbers. A
// catalog number the curated text does not carry is a difference: the
// seed rounds, or picked one firing mode.
func diffNumbers(params map[string]any, checks []paramCheck, values map[string]any) []Mismatch {
	out := []Mismatch{}
	for _, c := range checks {
		curated := docval.Value(params[c.param])
		if curated == "" {
			continue
		}
		catalogNums := catalogNumbers(values[c.field])
		if len(catalogNums) == 0 {
			continue
		}
		have := numbersIn(curated)
		missing := []string{}
		for _, n := range catalogNums {
			if !containsNumber(have, n) {
				missing = append(missing, trimNumber(n))
			}
		}
		if len(missing) == 0 {
			continue
		}
		out = append(out, Mismatch{
			Field:   "params." + c.param,
			Curated: curated,
			Catalog: strings.Join(missing, ", "),
		})
	}
	return out
}

// catalogNumbers reads the numeric values of a catalog field, which is a
// number or a list of them.
func catalogNumbers(v any) []float64 {
	switch t := v.(type) {
	case []any:
		var out []float64
		for _, e := range t {
			if n, ok := toFloat(e); ok {
				out = append(out, n)
			}
		}
		return out
	case nil:
		return nil
	}
	if n, ok := toFloat(v); ok {
		return []float64{n}
	}
	return nil
}

// diffAliases reports an alias the catalog knows and the curated list does
// not. The curated list may hold more (the seed's short forms), so only
// the catalog's gaps are reported.
func diffAliases(doc bson.M, it Item) *Mismatch {
	if len(it.Aliases) == 0 {
		return nil
	}
	curated := docval.List(doc, "aliases")
	known := map[string]bool{}
	for _, a := range curated {
		known[normalize(a)] = true
	}
	missing := []string{}
	for _, a := range it.Aliases {
		key := normalize(a)
		if key == "" || known[key] {
			continue
		}
		missing = append(missing, a)
	}
	if len(missing) == 0 {
		return nil
	}
	return &Mismatch{
		Field:   "aliases",
		Curated: strings.Join(curated, ", "),
		Catalog: strings.Join(missing, ", "),
	}
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
		out[key] = docval.Value(v)
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
