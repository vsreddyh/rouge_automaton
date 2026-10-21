package docval

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The store's readers read fields back in whatever shape the writer left
// them, and they must agree on how a value reads.
func TestValue(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"AR-11", "AR-11"},
		{45, "45"},
		{int64(45), "45"},
		{float64(45.5), "45.5"},
		{true, "true"},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := Value(tc.in); got != tc.want {
			t.Fatalf("Value(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestList(t *testing.T) {
	doc := bson.M{
		"aliases": bson.A{"hulk", "bruiser"},
		"role":    "walker",
		"empty":   []string{},
	}
	got := List(doc, "aliases")
	if len(got) != 2 || got[0] != "hulk" || got[1] != "bruiser" {
		t.Fatalf("list = %v", got)
	}
	if got := List(doc, "role"); len(got) != 1 || got[0] != "walker" {
		t.Fatalf("singleton = %v", got)
	}
	if got := List(doc, "missing"); got != nil {
		t.Fatalf("missing = %v", got)
	}
}
