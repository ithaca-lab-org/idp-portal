package secrets

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const opTimeout = 5 * time.Second

// MongoStore keeps secret metadata in the `secrets` collection. Filters are built from typed
// strings only, so query operators in a name are treated as literals.
type MongoStore struct{ c *mongo.Collection }

func NewMongoStore(db *mongo.Database) *MongoStore { return &MongoStore{c: db.Collection("secrets")} }

// EnsureIndexes creates the unique namespace+name index.
func (s *MongoStore) EnsureIndexes(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	_, err := s.c.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "namespace", Value: 1}, {Key: "name", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("namespace_name"),
	})
	return err
}

func key(ns, name string) bson.D {
	return bson.D{{Key: "namespace", Value: ns}, {Key: "name", Value: name}}
}

func (s *MongoStore) Upsert(ctx context.Context, m Metadata) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	keys := m.Keys
	if keys == nil {
		keys = []string{}
	}
	_, err := s.c.UpdateOne(ctx, key(m.Namespace, m.Name), bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "orderId", Value: m.OrderID},
			{Key: "keys", Value: keys},
			{Key: "version", Value: m.Version},
			{Key: "updatedBy", Value: m.UpdatedBy},
			{Key: "updatedAt", Value: m.UpdatedAt},
		}},
		{Key: "$setOnInsert", Value: bson.D{{Key: "createdAt", Value: m.UpdatedAt}}},
	}, options.UpdateOne().SetUpsert(true))
	return err
}

func (s *MongoStore) Get(ctx context.Context, ns, name, orderID string) (*Metadata, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	var m Metadata
	err := s.c.FindOne(ctx, append(key(ns, name), bson.E{Key: "orderId", Value: orderID})).Decode(&m)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *MongoStore) PurgeStale(ctx context.Context, ns, orderID string) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	_, err := s.c.DeleteMany(ctx, bson.D{{Key: "namespace", Value: ns}, {Key: "orderId", Value: bson.D{{Key: "$ne", Value: orderID}}}})
	return err
}

func (s *MongoStore) List(ctx context.Context, ns, orderID string, limit int) ([]Metadata, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	cur, err := s.c.Find(ctx, bson.D{{Key: "namespace", Value: ns}, {Key: "orderId", Value: orderID}},
		options.Find().SetSort(bson.D{{Key: "name", Value: 1}}).SetLimit(int64(limit)).SetProjection(bson.D{{Key: "_id", Value: 0}}))
	if err != nil {
		return nil, err
	}
	var out []Metadata
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *MongoStore) Delete(ctx context.Context, ns, name string) (*Metadata, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	var old Metadata
	err := s.c.FindOneAndDelete(ctx, key(ns, name)).Decode(&old)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &old, nil
}
