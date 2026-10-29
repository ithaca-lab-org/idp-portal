package secrets

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Metadata is everything stored about a secret outside Vault. There is no field for a value.
type Metadata struct {
	Namespace string `bson:"namespace" json:"-"`
	Name      string `bson:"name" json:"name"`
	// OrderID is the COMPLETE namespace order the secret was written under. Documents of an
	// earlier order (the namespace was released and ordered again) are treated as absent.
	OrderID   string    `bson:"orderId" json:"-"`
	Keys      []string  `bson:"keys" json:"keys"`
	Version   int       `bson:"version" json:"version"`
	UpdatedBy string    `bson:"updatedBy" json:"updatedBy"`
	UpdatedAt time.Time `bson:"updatedAt" json:"updatedAt"`
	CreatedAt time.Time `bson:"createdAt" json:"-"`
}

// Store keeps secret metadata, one document per namespace and name.
type Store interface {
	// Upsert replaces orderId, keys, version, updatedBy and updatedAt; createdAt is set on insert
	// only (callers PurgeStale first, so a document of another order is never updated in place).
	Upsert(ctx context.Context, m Metadata) error
	// Get returns the secret's metadata for orderID, or nil.
	Get(ctx context.Context, ns, name, orderID string) (*Metadata, error)
	// List returns the namespace's secrets of orderID ordered by name.
	List(ctx context.Context, ns, orderID string, limit int) ([]Metadata, error)
	// PurgeStale deletes the namespace's documents that belong to another order.
	PurgeStale(ctx context.Context, ns, orderID string) error
	// Delete removes the document and returns what was there (nil if nothing).
	Delete(ctx context.Context, ns, name string) (*Metadata, error)
}

// MemStore is an in-memory Store for tests.
type MemStore struct {
	mu    sync.Mutex
	items map[string]Metadata
}

func NewMemStore() *MemStore { return &MemStore{items: map[string]Metadata{}} }

func (m *MemStore) Upsert(_ context.Context, md Metadata) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := md.Namespace + "/" + md.Name
	if old, ok := m.items[k]; ok {
		md.CreatedAt = old.CreatedAt
	} else {
		md.CreatedAt = md.UpdatedAt
	}
	md.Keys = append([]string(nil), md.Keys...)
	m.items[k] = md
	return nil
}

func (m *MemStore) Get(_ context.Context, ns, name, orderID string) (*Metadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if md, ok := m.items[ns+"/"+name]; ok && md.OrderID == orderID {
		return &md, nil
	}
	return nil, nil
}

func (m *MemStore) PurgeStale(_ context.Context, ns, orderID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, md := range m.items {
		if md.Namespace == ns && md.OrderID != orderID {
			delete(m.items, k)
		}
	}
	return nil
}

func (m *MemStore) List(_ context.Context, ns, orderID string, limit int) ([]Metadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Metadata
	for _, md := range m.items {
		if md.Namespace == ns && md.OrderID == orderID {
			out = append(out, md)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemStore) Delete(_ context.Context, ns, name string) (*Metadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := ns + "/" + name
	old, ok := m.items[k]
	if !ok {
		return nil, nil
	}
	delete(m.items, k)
	return &old, nil
}
