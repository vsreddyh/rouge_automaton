package catalog

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// DefaultCacheCollection holds the cached catalog payloads. The whole
// catalog is ~1.4 MB, so one doc per collection is comfortably inside
// Mongo's 16 MB document limit.
const DefaultCacheCollection = "catalog_cache"

// Cache stores the last catalog payload per collection in Mongo. A catalog
// outage must cost freshness, not answers: with a cached snapshot the sync
// still runs against yesterday's data, and the seeds remain the floor when
// even that is missing.
type Cache struct {
	DB         *mongo.Database
	Collection string
}

// NewCache returns a cache backed by the given database.
func NewCache(db *mongo.Database) *Cache {
	return &Cache{DB: db, Collection: DefaultCacheCollection}
}

func (c *Cache) coll() string {
	if c.Collection != "" {
		return c.Collection
	}
	return DefaultCacheCollection
}

// cacheDoc is one cached collection payload.
type cacheDoc struct {
	ID          string    `bson:"_id"`
	ETag        string    `bson:"etag"`
	DataVersion string    `bson:"data_version"`
	FetchedAt   time.Time `bson:"fetched_at"`
	Count       int       `bson:"count"`
	Payload     string    `bson:"payload"`
}

// Load returns the cached snapshot and its ETag for one collection. found is
// false when nothing is cached yet.
func (c *Cache) Load(ctx context.Context, coll string) (Snapshot, string, bool, error) {
	if c == nil || c.DB == nil {
		return Snapshot{}, "", false, nil
	}
	var doc cacheDoc
	err := c.DB.Collection(c.coll()).FindOne(ctx, bson.M{"_id": coll}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return Snapshot{}, "", false, nil
		}
		return Snapshot{}, "", false, err
	}
	snap, err := Parse([]byte(doc.Payload))
	if err != nil {
		return Snapshot{}, "", false, err
	}
	return snap, doc.ETag, true, nil
}

// Save stores a snapshot and its validator.
func (c *Cache) Save(ctx context.Context, coll string, snap Snapshot, etag string) error {
	if c == nil || c.DB == nil {
		return nil
	}
	now := time.Now().UTC()
	// The _id is the collection name, so it is never part of the update.
	_, err := c.DB.Collection(c.coll()).UpdateOne(ctx,
		bson.M{"_id": coll},
		bson.M{"$set": bson.M{
			"etag":         etag,
			"data_version": snap.Version,
			"fetched_at":   now,
			"count":        snap.Count(),
			"payload":      string(snap.Raw),
		}},
		options.UpdateOne().SetUpsert(true))
	return err
}
