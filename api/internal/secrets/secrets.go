// Package secrets lets a team manage the Vault KV secrets of its own namespaces. The portal is a
// write-only Vault client: it writes new versions and deletes secrets, it never reads a value.
// Values exist only in the request being handled and in Vault; MongoDB, the ManagedSecret
// resource, logs, errors and audit events carry metadata only (names, key names, version, who).
package secrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ithaca-lab-org/idp-portal/api/internal/orders"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid request")
	// ErrExists is returned by a create-only Put when the secret already exists.
	ErrExists = errors.New("a secret with this name already exists")
	// ErrUpstream means Vault or the cluster failed. Its message is the only thing shown to the
	// caller; the cause is logged without values.
	ErrUpstream = errors.New("the secret backend request failed")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

const (
	MaxNameLen    = 40
	MaxKeys       = 20
	MaxValueBytes = 8192
	MaxTotalBytes = 64 << 10
	maxNamespaces = 200
	maxSecrets    = 500
)

var (
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	keyRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	// ReservedSecretNames and ReservedSecretPrefixes must equal the ManagedSecret CRD's "name is
	// reserved" rule (operator/api/v1alpha1/managedsecret_types.go); TestReservedNamesMatchTheCRD
	// fails when they drift. The web page gets them from GET /api/namespaces.
	ReservedSecretNames    = []string{"vault-reader", "default"}
	ReservedSecretPrefixes = []string{"default-token", "vault-reader-token", "sh-helm", "kube-", "istio-", "idp-"}
)

// validPathSegment is the check every namespace and secret name passes before it is placed in a
// Vault path or a resource name: a DNS label of at most 63 characters (no "/", "..", "%").
func validPathSegment(s string) bool { return len(s) <= 63 && dnsLabel.MatchString(s) }

// ValidateSecretName checks a secret name.
func ValidateSecretName(name string) error {
	if name == "" || len(name) > MaxNameLen || !dnsLabel.MatchString(name) {
		return invalid("name must be a lowercase DNS label of at most %d characters", MaxNameLen)
	}
	for _, r := range ReservedSecretNames {
		if name == r {
			return invalid("name %q is reserved", name)
		}
	}
	for _, p := range ReservedSecretPrefixes {
		if strings.HasPrefix(name, p) {
			return invalid("names starting with %q are reserved", p)
		}
	}
	return nil
}

// ValidateData checks the key names and value sizes. Messages never contain a value.
func ValidateData(data map[string]string) error {
	if len(data) < 1 || len(data) > MaxKeys {
		return invalid("a secret needs 1 to %d keys", MaxKeys)
	}
	total := 0
	for k, v := range data {
		if !keyRe.MatchString(k) {
			return invalid("key names must match %s", keyRe.String())
		}
		if len(v) < 1 || len(v) > MaxValueBytes {
			return invalid("the value of %s must be 1 to %d bytes", k, MaxValueBytes)
		}
		total += len(k) + len(v)
	}
	if total > MaxTotalBytes {
		return invalid("the secret is larger than %d bytes", MaxTotalBytes)
	}
	return nil
}

// Requester is the authenticated caller, taken from the verified session only.
type Requester = orders.Requester

// Namespaces looks up namespace orders; orders.MemStore and orders.MongoStore implement it.
type Namespaces interface {
	CompleteNamespace(ctx context.Context, name string) (*orders.Order, error)
	CompleteNamespacesFor(ctx context.Context, bus []string, limit int) ([]orders.Order, error)
}

// Namespace is one namespace the caller may manage secrets in.
type Namespace struct {
	Name         string `json:"name"`
	BusinessUnit string `json:"businessUnit"`
}

// Result is what a write returns: never the values.
type Result struct {
	Name    string   `json:"name"`
	Keys    []string `json:"keys"`
	Version int      `json:"version"`
}

type Service struct {
	Vault      Vault
	Cluster    Cluster
	Store      Store
	Namespaces Namespaces
	Events     Publisher // optional
	Now        func() time.Time

	// locks serialize writes and deletes of one secret within this process, so the metadata
	// cannot record an older version after a newer one. The portal runs a single replica.
	locksMu sync.Mutex
	locks   map[string]*keyLock
}

type keyLock struct {
	mu   sync.Mutex
	refs int
}

func NewService(v Vault, c Cluster, s Store, n Namespaces) *Service {
	return &Service{Vault: v, Cluster: c, Store: s, Namespaces: n, Now: time.Now, locks: map[string]*keyLock{}}
}

func (s *Service) lock(ns, name string) func() {
	k := ns + "/" + name
	s.locksMu.Lock()
	l := s.locks[k]
	if l == nil {
		l = &keyLock{}
		s.locks[k] = l
	}
	l.refs++
	s.locksMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		s.locksMu.Lock()
		if l.refs--; l.refs == 0 {
			delete(s.locks, k)
		}
		s.locksMu.Unlock()
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// ListNamespaces returns the namespaces the caller may manage: COMPLETE namespace orders of one
// of the caller's groups.
func (s *Service) ListNamespaces(ctx context.Context, who Requester) ([]Namespace, error) {
	out := []Namespace{}
	if who.Sub == "" || len(who.Groups) == 0 {
		return out, nil
	}
	list, err := s.Namespaces.CompleteNamespacesFor(ctx, who.Groups, maxNamespaces)
	if err != nil {
		return nil, err
	}
	for _, o := range list {
		out = append(out, Namespace{Name: o.Name, BusinessUnit: o.BusinessUnit})
	}
	return out, nil
}

// authorize returns ErrNotFound unless ns is a COMPLETE namespace order of one of the caller's
// groups. A namespace that does not exist and one the caller may not touch look the same.
// It returns the id of that order: metadata belongs to one order, so a namespace that is
// released and ordered again (maybe by another business unit) starts with no secrets listed.
func (s *Service) authorize(ctx context.Context, who Requester, ns string) (string, error) {
	if who.Sub == "" || !validPathSegment(ns) || orders.ValidateName(ns) != nil {
		return "", ErrNotFound
	}
	o, err := s.Namespaces.CompleteNamespace(ctx, ns)
	if errors.Is(err, orders.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if !contains(who.Groups, o.BusinessUnit) || o.ID == "" {
		return "", ErrNotFound
	}
	return o.ID, nil
}

func (s *Service) List(ctx context.Context, who Requester, ns string) ([]Metadata, error) {
	orderID, err := s.authorize(ctx, who, ns)
	if err != nil {
		return nil, err
	}
	// metadata left by an earlier owner of this namespace name is never shown; drop it
	if err := s.Store.PurgeStale(ctx, ns, orderID); err != nil {
		return nil, err
	}
	list, err := s.Store.List(ctx, ns, orderID, maxSecrets)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []Metadata{}
	}
	return list, nil
}

func sortedKeys(data map[string]string) []string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func actor(who Requester) string { return strings.ToLower(strings.TrimSpace(who.Email)) }

// Put replaces the whole secret with data (a new KV version); with create set it refuses
// (ErrExists) to replace a secret that already exists. Order: the ManagedSecret resource first,
// so a name the CRD rejects never reaches Vault; then Vault; then the metadata, so the metadata
// never claims a version Vault does not have. A resource without a value only shows a sync
// error in the operator/VSO until the write lands. If the Vault write fails the answer is
// ErrUpstream and a retry is idempotent (the resource already exists).
func (s *Service) Put(ctx context.Context, who Requester, ns, name string, data map[string]string, create bool) (*Result, error) {
	orderID, err := s.authorize(ctx, who, ns)
	if err != nil {
		return nil, err
	}
	if err := ValidateSecretName(name); err != nil {
		return nil, err
	}
	if err := ValidateData(data); err != nil {
		return nil, err
	}
	unlock := s.lock(ns, name)
	defer unlock()

	if err := s.Store.PurgeStale(ctx, ns, orderID); err != nil {
		return nil, err
	}
	if create {
		if old, err := s.Store.Get(ctx, ns, name, orderID); err != nil {
			return nil, err
		} else if old != nil {
			return nil, ErrExists
		}
	}
	if err := s.Cluster.EnsureManagedSecret(ctx, ns, name); err != nil {
		if errors.Is(err, ErrInvalid) {
			return nil, err
		}
		slog.Error("create managedsecret", "namespace", ns, "name", name, "err", err)
		return nil, ErrUpstream
	}
	version, err := s.Vault.Write(ctx, ns, name, data)
	if err != nil {
		slog.Error("vault write", "namespace", ns, "name", name, "err", err)
		return nil, ErrUpstream
	}
	keys := sortedKeys(data)
	now := s.Now().UTC().Truncate(time.Millisecond)
	m := Metadata{Namespace: ns, Name: name, OrderID: orderID, Keys: keys, Version: version, UpdatedBy: actor(who), UpdatedAt: now}
	if err := s.Store.Upsert(ctx, m); err != nil {
		return nil, err
	}
	s.emit(ctx, EventChanged, m)
	return &Result{Name: name, Keys: keys, Version: version}, nil
}

// Delete removes the ManagedSecret resource, every Vault version and the metadata. Each step
// tolerates "already gone", so a retry after a partial failure finishes the job.
func (s *Service) Delete(ctx context.Context, who Requester, ns, name string) error {
	orderID, err := s.authorize(ctx, who, ns)
	if err != nil {
		return err
	}
	if err := ValidateSecretName(name); err != nil {
		return ErrNotFound
	}
	unlock := s.lock(ns, name)
	defer unlock()

	if err := s.Cluster.DeleteManagedSecret(ctx, ns, name); err != nil {
		slog.Error("delete managedsecret", "namespace", ns, "name", name, "err", err)
		return ErrUpstream
	}
	if err := s.Vault.DeleteAll(ctx, ns, name); err != nil {
		slog.Error("vault delete", "namespace", ns, "name", name, "err", err)
		return ErrUpstream
	}
	old, err := s.Store.Delete(ctx, ns, name)
	if err != nil {
		return err
	}
	m := Metadata{Namespace: ns, Name: name, OrderID: orderID, UpdatedBy: actor(who), UpdatedAt: s.Now().UTC()}
	if old != nil && old.OrderID == orderID { // never put a previous owner's key names in the event
		m.Keys, m.Version = old.Keys, old.Version
	}
	s.emit(ctx, EventDeleted, m)
	return nil
}
