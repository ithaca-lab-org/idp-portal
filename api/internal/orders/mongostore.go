package orders

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const opTimeout = 5 * time.Second

// MongoStore stores orders in MongoDB. Every filter is built from typed values (strings are
// never decoded into query documents), so operator injection such as {"$ne": ""} cannot happen.
type MongoStore struct {
	orders   *mongo.Collection
	counters *mongo.Collection
	now      func() time.Time
}

func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{orders: db.Collection("orders"), counters: db.Collection("counters"), now: time.Now}
}

// EnsureIndexes creates the unique partial index that allows one active order per name.
func (s *MongoStore) EnsureIndexes(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	_, err := s.orders.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys: bson.D{{Key: "kind", Value: 1}, {Key: "name", Value: 1}},
			Options: options.Index().SetUnique(true).SetName("active_name").
				SetPartialFilterExpression(bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{string(StatusPending), string(StatusComplete)}}}}}),
		},
		{Keys: bson.D{{Key: "owner", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("owner_created")},
		{Keys: bson.D{{Key: "status", Value: 1}}, Options: options.Index().SetName("status")},
	})
	return err
}

func (s *MongoStore) nextSeq(ctx context.Context, year int) (int64, error) {
	var doc struct {
		Seq int64 `bson:"seq"`
	}
	err := s.counters.FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: "orders-" + itoa(year)}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "seq", Value: int64(1)}}}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&doc)
	return doc.Seq, err
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

func (s *MongoStore) Create(ctx context.Context, o *Order) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	now := s.now().UTC().Truncate(time.Millisecond)
	seq, err := s.nextSeq(ctx, now.Year())
	if err != nil {
		return err
	}
	o.ID = orderID(now.Year(), seq)
	o.CreatedAt, o.UpdatedAt = now, now
	if _, err := s.orders.InsertOne(ctx, o); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return ErrNameTaken
		}
		return err
	}
	return nil
}

func (s *MongoStore) Get(ctx context.Context, id string) (*Order, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	var o Order
	err := s.orders.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&o)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	return &o, err
}

func (s *MongoStore) list(ctx context.Context, filter bson.D, limit int) ([]Order, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	cur, err := s.orders.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	var out []Order
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *MongoStore) ListByOwner(ctx context.Context, owner string, limit int) ([]Order, error) {
	return s.list(ctx, bson.D{{Key: "owner", Value: owner}}, limit)
}

func (s *MongoStore) ListPending(ctx context.Context, limit int) ([]Order, error) {
	return s.list(ctx, bson.D{{Key: "status", Value: string(StatusPending)}}, limit)
}

func (s *MongoStore) UpdateStatus(ctx context.Context, id string, to Status, message string) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	res, err := s.orders.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "status", Value: string(StatusPending)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: string(to)}, {Key: "message", Value: message}, {Key: "updatedAt", Value: s.now().UTC()}}}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoStore) countActive(ctx context.Context, field, value string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	n, err := s.orders.CountDocuments(ctx, bson.D{
		{Key: field, Value: value},
		{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{string(StatusPending), string(StatusComplete)}}}},
	})
	return int(n), err
}

func (s *MongoStore) CountActiveByBusinessUnit(ctx context.Context, bu string) (int, error) {
	return s.countActive(ctx, "businessUnit", bu)
}

func (s *MongoStore) CountActiveByOwner(ctx context.Context, owner string) (int, error) {
	return s.countActive(ctx, "owner", owner)
}
