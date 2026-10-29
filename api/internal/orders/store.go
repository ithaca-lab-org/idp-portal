package orders

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Store persists orders. Create assigns ID, CreatedAt and UpdatedAt and must return ErrNameTaken
// when a non-FAILED order for the same kind and name exists.
type Store interface {
	Create(ctx context.Context, o *Order) error
	Get(ctx context.Context, id string) (*Order, error)
	ListByOwner(ctx context.Context, ownerSub string, limit int) ([]Order, error)
	ListByStatus(ctx context.Context, status Status, limit int) ([]Order, error)
	// UpdateStatus changes the status only if the order is currently in `from`; it returns
	// ErrNotFound otherwise, so concurrent updaters cannot overwrite each other.
	UpdateStatus(ctx context.Context, id string, from, to Status, message string) error
	CountActiveByBusinessUnit(ctx context.Context, bu string) (int, error)
	CountActiveByOwner(ctx context.Context, ownerSub string) (int, error)
}

// active orders hold their name and count against the caps.
func active(st Status) bool { return st == StatusPending || st == StatusComplete }

func orderID(year int, seq int64) string { return fmt.Sprintf("ORD-%d-%06d", year, seq) }

// MemStore is an in-memory Store for tests.
type MemStore struct {
	mu     sync.Mutex
	orders map[string]*Order
	seq    map[int]int64
	Now    func() time.Time
}

func NewMemStore() *MemStore {
	return &MemStore{orders: map[string]*Order{}, seq: map[int]int64{}, Now: time.Now}
}

func (m *MemStore) Create(_ context.Context, o *Order) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.orders {
		if e.Kind == o.Kind && e.Name == o.Name && active(e.Status) {
			return ErrNameTaken
		}
	}
	now := m.Now().UTC()
	m.seq[now.Year()]++
	o.ID = orderID(now.Year(), m.seq[now.Year()])
	o.CreatedAt, o.UpdatedAt = now, now
	c := *o
	m.orders[o.ID] = &c
	return nil
}

func (m *MemStore) Get(_ context.Context, id string) (*Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.orders[id]; ok {
		c := *o
		return &c, nil
	}
	return nil, ErrNotFound
}

func (m *MemStore) filter(pred func(*Order) bool, limit int) []Order {
	var out []Order
	for _, o := range m.orders {
		if pred(o) {
			out = append(out, *o)
		}
	}
	sort.Slice(out, func(a, b int) bool { // newest first, id as the tie-breaker
		if !out[a].CreatedAt.Equal(out[b].CreatedAt) {
			return out[a].CreatedAt.After(out[b].CreatedAt)
		}
		return out[a].ID > out[b].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (m *MemStore) ListByOwner(_ context.Context, ownerSub string, limit int) ([]Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filter(func(o *Order) bool { return o.OwnerSub == ownerSub }, limit), nil
}

func (m *MemStore) ListByStatus(_ context.Context, status Status, limit int) ([]Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filter(func(o *Order) bool { return o.Status == status }, limit), nil
}

func (m *MemStore) UpdateStatus(_ context.Context, id string, from, to Status, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.orders[id]
	if !ok || o.Status != from {
		return ErrNotFound
	}
	o.Status, o.Message, o.UpdatedAt = to, message, m.Now().UTC()
	return nil
}

func (m *MemStore) count(pred func(*Order) bool) int {
	n := 0
	for _, o := range m.orders {
		if active(o.Status) && pred(o) {
			n++
		}
	}
	return n
}

func (m *MemStore) CountActiveByBusinessUnit(_ context.Context, bu string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count(func(o *Order) bool { return o.BusinessUnit == bu }), nil
}

func (m *MemStore) CountActiveByOwner(_ context.Context, ownerSub string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count(func(o *Order) bool { return o.OwnerSub == ownerSub }), nil
}

// CompleteNamespace returns the COMPLETE namespace order for name, or ErrNotFound. The unique
// active-name index guarantees there is at most one.
func (m *MemStore) CompleteNamespace(_ context.Context, name string) (*Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.orders {
		if o.Kind == KindNamespace && o.Name == name && o.Status == StatusComplete {
			c := *o
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

// CompleteNamespacesFor lists COMPLETE namespace orders whose business unit is one of bus,
// ordered by name.
func (m *MemStore) CompleteNamespacesFor(_ context.Context, bus []string, limit int) ([]Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Order
	for _, o := range m.orders {
		if o.Kind == KindNamespace && o.Status == StatusComplete && contains(bus, o.BusinessUnit) {
			out = append(out, *o)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
