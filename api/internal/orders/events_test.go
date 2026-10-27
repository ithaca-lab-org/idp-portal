package orders

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakePub struct {
	mu  sync.Mutex
	got []Event
	err error
}

func (f *fakePub) Publish(_ context.Context, e Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, e)
	return f.err
}

func eventSvc(t *testing.T) (*Service, *MemStore, *fakeCluster, *fakePub) {
	t.Helper()
	store, cluster, pub := NewMemStore(), newFakeCluster(), &fakePub{}
	svc := NewService(store, cluster, 10, 5)
	svc.Events = pub
	return svc, store, cluster, pub
}

func place(t *testing.T, svc *Service) *Order {
	t.Helper()
	o, err := svc.PlaceNamespaceOrder(context.Background(),
		Requester{Sub: "u1", Email: "A@x.com", Groups: []string{"payment-bu"}},
		NamespaceRequest{Name: "team-a", BusinessUnit: "payment-bu", Size: "small"})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestCompleteOrderPublishesOneEvent(t *testing.T) {
	svc, _, cluster, pub := eventSvc(t)
	o := place(t, svc)
	cluster.set("team-a", "Ready", "")
	svc.SyncOnce(context.Background(), false)
	svc.SyncOnce(context.Background(), false) // already COMPLETE: no second event
	if len(pub.got) != 1 {
		t.Fatalf("events: %+v", pub.got)
	}
	e := pub.got[0]
	if e.Type != EventComplete || e.OrderID != o.ID || e.Owner != "a@x.com" || e.Version != 1 || e.Status != StatusComplete {
		t.Fatalf("event %+v", e)
	}
}

func TestFailedOrderPublishesWithMessage(t *testing.T) {
	svc, _, cluster, pub := eventSvc(t)
	place(t, svc)
	cluster.set("team-a", "Failed", "quota exceeded")
	svc.SyncOnce(context.Background(), false)
	if len(pub.got) != 1 || pub.got[0].Type != EventFailed || pub.got[0].Message != "quota exceeded" {
		t.Fatalf("events: %+v", pub.got)
	}
}

func TestReleasedAndPendingDoNotPublish(t *testing.T) {
	svc, _, cluster, pub := eventSvc(t)
	place(t, svc)
	svc.SyncOnce(context.Background(), false) // still PENDING
	cluster.set("team-a", "Ready", "")
	svc.SyncOnce(context.Background(), false)
	pub.got = nil
	delete(cluster.items, "team-a")
	svc.SyncOnce(context.Background(), true) // COMPLETE -> RELEASED
	if len(pub.got) != 0 {
		t.Fatalf("unexpected events: %+v", pub.got)
	}
}

func TestPublishFailureDoesNotFailTheOrder(t *testing.T) {
	svc, store, cluster, pub := eventSvc(t)
	pub.err = errors.New("bus down")
	o := place(t, svc)
	cluster.set("team-a", "Ready", "")
	svc.SyncOnce(context.Background(), false)
	got, _ := store.Get(context.Background(), o.ID)
	if got.Status != StatusComplete {
		t.Fatalf("status %s", got.Status)
	}
}

func TestNoPublisherIsFine(t *testing.T) {
	svc, _, cluster, _ := eventSvc(t)
	svc.Events = nil
	place(t, svc)
	cluster.set("team-a", "Ready", "")
	svc.SyncOnce(context.Background(), false) // must not panic
}

func TestEventMessageIsCapped(t *testing.T) {
	e, _ := newEvent(&Order{ID: "ORD-2026-000001"}, StatusFailed, string(make([]byte, 500)), time.Now())
	if len(e.Message) != maxEventMessage {
		t.Fatalf("len %d", len(e.Message))
	}
}
