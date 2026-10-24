package orders

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store persists orders. Create assigns ID, CreatedAt and UpdatedAt and must return ErrNameTaken
// when a non-FAILED order for the same kind and name exists.
type Store interface {
	Create(ctx context.Context, o *Order) error
	Get(ctx context.Context, id string) (*Order, error)
	ListByOwner(ctx context.Context, owner string, limit int) ([]Order, error)
	ListPending(ctx context.Context, limit int) ([]Order, error)
	// UpdateStatus moves an order out of PENDING; it returns ErrNotFound if it is not PENDING.
	UpdateStatus(ctx context.Context, id string, to Status, message string) error
	CountActiveByBusinessUnit(ctx context.Context, bu string) (int, error)
	CountActiveByOwner(ctx context.Context, owner string) (int, error)
}

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
		if e.Kind == o.Kind && e.Name == o.Name && e.Status != StatusFailed {
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

func (m *MemStore) ListByOwner(_ context.Context, owner string, limit int) ([]Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filter(func(o *Order) bool { return strings.EqualFold(o.Owner, owner) }, limit), nil
}

func (m *MemStore) ListPending(_ context.Context, limit int) ([]Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filter(func(o *Order) bool { return o.Status == StatusPending }, limit), nil
}

func (m *MemStore) UpdateStatus(_ context.Context, id string, to Status, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.orders[id]
	if !ok || o.Status != StatusPending {
		return ErrNotFound
	}
	o.Status, o.Message, o.UpdatedAt = to, message, m.Now().UTC()
	return nil
}

func (m *MemStore) count(pred func(*Order) bool) int {
	n := 0
	for _, o := range m.orders {
		if o.Status != StatusFailed && pred(o) {
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

func (m *MemStore) CountActiveByOwner(_ context.Context, owner string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count(func(o *Order) bool { return strings.EqualFold(o.Owner, owner) }), nil
}
