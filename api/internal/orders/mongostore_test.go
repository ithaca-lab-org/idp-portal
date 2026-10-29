package orders

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// These tests need a real MongoDB: set MONGO_TEST_URI (CI starts a service container).
func newMongoStore(t *testing.T) *MongoStore {
	t.Helper()
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("MONGO_TEST_URI not set")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetServerSelectionTimeout(5 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("idp_test_%d", time.Now().UnixNano())
	db := client.Database(dbName)
	t.Cleanup(func() {
		_ = db.Drop(context.Background())
		_ = client.Disconnect(context.Background())
	})
	s := NewMongoStore(db)
	if err := s.EnsureIndexes(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func newOrder(name, owner string) *Order {
	return &Order{Kind: KindNamespace, Name: name, BusinessUnit: "payment-bu", Owner: owner, OwnerSub: "sub-" + owner, Size: "small", Status: StatusPending}
}

func TestMongoCounterIsAtomicUnderConcurrency(t *testing.T) {
	s := newMongoStore(t)
	ctx := context.Background()
	const n = 40
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o := newOrder(fmt.Sprintf("team-%d", i), "a@example.com")
			if err := s.Create(ctx, o); err != nil {
				t.Error(err)
				return
			}
			ids <- o.ID
		}(i)
	}
	wg.Wait()
	close(ids)
	var got []string
	for id := range ids {
		got = append(got, id)
	}
	sort.Strings(got)
	for i, id := range got {
		want := orderID(time.Now().UTC().Year(), int64(i+1))
		if id != want {
			t.Fatalf("ids must be unique and gapless, position %d: got %s want %s", i, id, want)
		}
	}
	if len(got) != n {
		t.Fatalf("created %d of %d", len(got), n)
	}
}

func TestMongoOneActiveOrderPerName(t *testing.T) {
	s := newMongoStore(t)
	ctx := context.Background()
	a := newOrder("shared", "a@example.com")
	if err := s.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, newOrder("shared", "b@example.com")); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("pending duplicate: %v", err)
	}
	if err := s.UpdateStatus(ctx, a.ID, StatusPending, StatusComplete, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, newOrder("shared", "b@example.com")); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("complete duplicate: %v", err)
	}
	f := newOrder("retry", "a@example.com")
	_ = s.Create(ctx, f)
	if err := s.UpdateStatus(ctx, f.ID, StatusPending, StatusFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, newOrder("retry", "a@example.com")); err != nil {
		t.Fatalf("a FAILED order must free the name: %v", err)
	}
}

func TestMongoStatusAndCounts(t *testing.T) {
	s := newMongoStore(t)
	ctx := context.Background()
	a, b := newOrder("one", "a@example.com"), newOrder("two", "a@example.com")
	_ = s.Create(ctx, a)
	_ = s.Create(ctx, b)
	_ = s.UpdateStatus(ctx, b.ID, StatusPending, StatusFailed, "x")
	if err := s.UpdateStatus(ctx, b.ID, StatusPending, StatusComplete, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a finished order must not change again: %v", err)
	}
	if n, _ := s.CountActiveByBusinessUnit(ctx, "payment-bu"); n != 1 {
		t.Fatalf("active by bu: %d", n)
	}
	if n, _ := s.CountActiveByOwner(ctx, "sub-a@example.com"); n != 1 {
		t.Fatalf("active by owner: %d", n)
	}
	pending, _ := s.ListByStatus(ctx, StatusPending, 10)
	if len(pending) != 1 || pending[0].ID != a.ID {
		t.Fatalf("pending: %+v", pending)
	}
	list, _ := s.ListByOwner(ctx, "sub-a@example.com", 10)
	if len(list) != 2 || list[0].ID != b.ID {
		t.Fatalf("newest first: %+v", list)
	}
	got, err := s.Get(ctx, a.ID)
	if err != nil || got.Name != "one" {
		t.Fatalf("get: %v %v", got, err)
	}
}

func TestMongoQueryOperatorsAreTreatedAsLiterals(t *testing.T) {
	s := newMongoStore(t)
	ctx := context.Background()
	_ = s.Create(ctx, newOrder("one", "victim@example.com"))
	for _, evil := range []string{`{"$ne":""}`, `{"$gt":""}`, `{"$regex":".*"}`, `victim@example.com' || '1'=='1`} {
		list, err := s.ListByOwner(ctx, evil, 10)
		if err != nil || len(list) != 0 {
			t.Errorf("owner %q must match nothing, got %v %v", evil, list, err)
		}
		if _, err := s.Get(ctx, evil); !errors.Is(err, ErrNotFound) {
			t.Errorf("id %q: %v", evil, err)
		}
		if n, _ := s.CountActiveByBusinessUnit(ctx, evil); n != 0 {
			t.Errorf("bu %q counted %d", evil, n)
		}
	}
}

func TestMongoReleasedOrdersFreeTheNameAndTheCap(t *testing.T) {
	s := newMongoStore(t)
	ctx := context.Background()
	o := newOrder("gone", "a@example.com")
	_ = s.Create(ctx, o)
	if err := s.UpdateStatus(ctx, o.ID, StatusPending, StatusComplete, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, o.ID, StatusPending, StatusReleased, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the from-status guard must stop a stale transition: %v", err)
	}
	if err := s.UpdateStatus(ctx, o.ID, StatusComplete, StatusReleased, "removed"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountActiveByOwner(ctx, "sub-a@example.com"); n != 0 {
		t.Fatalf("a released order must not count: %d", n)
	}
	if err := s.Create(ctx, newOrder("gone", "b@example.com")); err != nil {
		t.Fatalf("a released order must free the name: %v", err)
	}
}

func TestMongoCompleteNamespaceLookups(t *testing.T) {
	s := newMongoStore(t)
	ctx := context.Background()
	mk := func(name, bu string, to Status) {
		o := newOrder(name, "a@example.com")
		o.BusinessUnit = bu
		if err := s.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
		if to != StatusPending {
			if err := s.UpdateStatus(ctx, o.ID, StatusPending, to, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("team-b", "payment-bu", StatusComplete)
	mk("team-a", "payment-bu", StatusComplete)
	mk("team-p", "payment-bu", StatusPending)
	mk("team-x", "other-bu", StatusComplete)
	if o, err := s.CompleteNamespace(ctx, "team-a"); err != nil || o.BusinessUnit != "payment-bu" {
		t.Fatalf("%v %v", o, err)
	}
	if _, err := s.CompleteNamespace(ctx, "team-p"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a PENDING order must not count: %v", err)
	}
	list, err := s.CompleteNamespacesFor(ctx, []string{"payment-bu"}, 50)
	if err != nil || len(list) != 2 || list[0].Name != "team-a" || list[1].Name != "team-b" {
		t.Fatalf("%v %v", list, err)
	}
	if list, err := s.CompleteNamespacesFor(ctx, nil, 50); err != nil || len(list) != 0 {
		t.Fatalf("no groups must list nothing: %v %v", list, err)
	}
}
