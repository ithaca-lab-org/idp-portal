package secrets

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// These tests need a real MongoDB: set MONGO_TEST_URI (CI starts a service container).
func newMongoStore(t *testing.T) (*MongoStore, *mongo.Database) {
	t.Helper()
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("MONGO_TEST_URI not set")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetServerSelectionTimeout(5 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	db := client.Database(fmt.Sprintf("idp_secrets_test_%d", time.Now().UnixNano()))
	t.Cleanup(func() {
		_ = db.Drop(context.Background())
		_ = client.Disconnect(context.Background())
	})
	s := NewMongoStore(db)
	if err := s.EnsureIndexes(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, db
}

func TestMongoSecretsUpsertListDelete(t *testing.T) {
	s, db := newMongoStore(t)
	ctx := context.Background()
	t1 := time.Unix(1000, 0).UTC()
	t2 := t1.Add(time.Hour)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Upsert(ctx, Metadata{Namespace: "team-a", Name: "db", Keys: []string{"A", "B"}, Version: 1, UpdatedBy: "a@x.com", UpdatedAt: t1}))
	must(s.Upsert(ctx, Metadata{Namespace: "team-a", Name: "db", Keys: []string{"C"}, Version: 2, UpdatedBy: "b@x.com", UpdatedAt: t2}))
	must(s.Upsert(ctx, Metadata{Namespace: "team-a", Name: "api", Keys: []string{"K"}, Version: 1, UpdatedBy: "a@x.com", UpdatedAt: t1}))
	must(s.Upsert(ctx, Metadata{Namespace: "team-b", Name: "db", Keys: []string{"K"}, Version: 7, UpdatedBy: "a@x.com", UpdatedAt: t1}))
	l, err := s.List(ctx, "team-a", 50)
	must(err)
	if len(l) != 2 || l[0].Name != "api" || l[1].Name != "db" || l[1].Version != 2 || l[1].UpdatedBy != "b@x.com" ||
		len(l[1].Keys) != 1 || !l[1].CreatedAt.Equal(t1) || !l[1].UpdatedAt.Equal(t2) {
		t.Fatalf("%+v", l)
	}
	// one document per namespace+name, and only the metadata fields
	n, _ := db.Collection("secrets").CountDocuments(ctx, bson.D{})
	if n != 3 {
		t.Fatalf("%d documents", n)
	}
	var raw bson.M
	must(db.Collection("secrets").FindOne(ctx, bson.D{{Key: "namespace", Value: "team-a"}, {Key: "name", Value: "db"}}).Decode(&raw))
	for k := range raw {
		switch k {
		case "_id", "namespace", "name", "keys", "version", "updatedBy", "updatedAt", "createdAt":
		default:
			t.Errorf("unexpected field %q", k)
		}
	}
	// operators in a name are literals
	if l, _ := s.List(ctx, `{"$ne":""}`, 50); len(l) != 0 {
		t.Fatal("operator injection")
	}
	old, err := s.Delete(ctx, "team-a", "db")
	must(err)
	if old == nil || old.Version != 2 {
		t.Fatalf("%+v", old)
	}
	if old, err := s.Delete(ctx, "team-a", "db"); err != nil || old != nil {
		t.Fatalf("second delete: %v %v", old, err)
	}
	// a duplicate insert is refused by the unique index
	_, err = db.Collection("secrets").InsertOne(ctx, bson.D{{Key: "namespace", Value: "team-b"}, {Key: "name", Value: "db"}})
	if !mongo.IsDuplicateKeyError(err) {
		t.Fatalf("unique index missing: %v", err)
	}
}
