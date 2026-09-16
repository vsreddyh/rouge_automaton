package rag

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The router is gone by design (the model decides what to look up), so the
// tests pin the two remaining contracts: the faction allowlist and ranking.
func TestFactions(t *testing.T) {
	for _, f := range []string{"automatons", "terminids", "illuminate"} {
		if !Factions[f] {
			t.Fatalf("%q must be a valid faction", f)
		}
	}
	for _, f := range []string{"bots", "squids", ""} {
		if Factions[f] {
			t.Fatalf("%q must not be a valid faction", f)
		}
	}
}

func TestScore(t *testing.T) {
	words := Keywords("how to kill hulk bruiser")
	hulk := bson.M{"name": "Hulk Bruiser", "aliases": bson.A{"hulk"},
		"text_blob": "Hulk Bruiser | automatons | Massive walker"}
	other := bson.M{"name": "Agitator", "aliases": bson.A{"radical"},
		"text_blob": "Agitator | automatons | how to kill the radical"}
	if Score("how to kill hulk bruiser", words, hulk) <= Score("how to kill hulk bruiser", words, other) {
		t.Fatal("Hulk Bruiser must outrank Agitator")
	}
	// Missing text_blob must not panic (review #7.1).
	bare := bson.M{"name": "Mystery"}
	if s := Score("mystery", Keywords("mystery"), bare); s < 10 {
		t.Fatalf("name hit bonus must apply without text_blob, got %v", s)
	}
	if got := Keywords("hulk hulk hulk"); len(got) != 1 {
		t.Fatalf("keywords must dedupe, got %v", got)
	}
}
