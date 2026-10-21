// Package docval renders document values as text. The store's readers —
// the seed loader, the crawler, the catalog sync — all read fields back
// out of Mongo, and they must agree on how a value reads.
//
// The rendering contract here is a *reading* contract: a value becomes the
// text it stands for. The model-facing renderer in internal/mcpdb is a
// different contract on purpose (a value that is a map or a slice reads
// empty there, so it never leaks into an answer).
package docval

import (
	"fmt"
	"strconv"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Value renders a document value as one line of text: strings pass
// through, numbers and booleans render as themselves, and anything else
// renders as its Go form. A nil value reads empty.
func Value(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int32:
		return strconv.Itoa(int(t))
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return fmt.Sprint(v)
}

// List reads a list field in any writer shape: a BSON array, a Go slice,
// a singleton or a single string. The shape depends on who wrote the doc,
// so every reader goes through this one switch.
func List(doc bson.M, key string) []string {
	switch t := doc[key].(type) {
	case bson.A:
		var out []string
		for _, e := range t {
			if s := Value(e); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		var out []string
		for _, e := range t {
			if s := Value(e); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	case string:
		return []string{t}
	}
	return nil
}
