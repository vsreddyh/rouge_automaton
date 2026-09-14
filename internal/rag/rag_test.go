package rag

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRoute(t *testing.T) {
	if got := Route("how to kill hulk"); !got["U"] || len(got) != 1 {
		t.Fatalf("hulk route = %v", got)
	}
	if got := Route("best primary vs terminids"); !got["U"] || !got["G"] {
		t.Fatalf("primary route = %v", got)
	}
	if got := Route("who owns cyberstan right now"); !got["L"] {
		t.Fatalf("live route = %v", got)
	}
	if got := Route("tundra conditions"); !got["W"] {
		t.Fatalf("world route = %v", got)
	}
}

func TestFaction(t *testing.T) {
	if FactionOf("harvester shield") != "illuminate" {
		t.Fatal("harvester must map to illuminate")
	}
	if FactionOf("orbital strike") != "" {
		t.Fatal("gear query must have no faction")
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
