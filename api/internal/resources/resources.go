// Package resources lets a team order cloud buckets (BucketClaim) and give a workload access to
// one (ResourceGrant) in its own namespaces (ADR 0010). The two resources are the record: the
// portal writes a name and a fixed menu choice, the resource broker (a separate Deployment)
// creates the bucket and the IAM binding and reports back in the status. Nothing is stored in
// MongoDB and no secret is involved; no cloud error text ever reaches a tenant.
//
// There is no bucket delete in v1: deleting a claim can orphan a bucket that still holds data,
// so a bucket is retired by the platform team. The only delete is revoking a grant.
package resources

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"github.com/ithaca-lab-org/idp-portal/api/internal/orders"
	"github.com/ithaca-lab-org/idp-portal/api/internal/secrets"
	"github.com/ithaca-lab-org/idp-portal/api/internal/workloads"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid request")
	// ErrNamespaceUnsupported means the caller's own namespace cannot own buckets (name too long
	// for a bucket name, or one Cloud Storage refuses).
	ErrNamespaceUnsupported = errors.New("this namespace cannot own buckets")
	ErrBucketExists         = errors.New("a bucket with this name already exists")
	ErrGrantExists          = errors.New("this workload already has a grant on this bucket")
	// ErrGrantCollision means the grant name <workload>-<bucket> is taken by a grant for another
	// pair (workload a-b on bucket c, and workload a on bucket b-c, share a name).
	ErrGrantCollision = errors.New("the grant name collides with an existing grant")
	ErrUpstream       = errors.New("the cluster request failed")
	ErrBucketLimit    = errors.New("this namespace has reached its bucket limit")
	ErrGrantLimit     = errors.New("this namespace has reached its grant limit")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

const (
	StatusPending  = "PENDING"
	StatusComplete = "COMPLETE"
	StatusFailed   = "FAILED"

	AccessReader = "reader"
	AccessWriter = "writer"

	// MaxNameLen and MaxNamespaceLen keep idp-<namespace>-<claim>-<8 hex> within the 63
	// characters of a bucket name: 4 + 31 + 1 + 18 + 1 + 8 = 63. They are the BucketClaim CRD's
	// and the broker's limits (operator/internal/broker/rules.go); TestRulesMatchTheCRDAndTheBroker
	// fails when they drift.
	MaxNameLen      = 18
	MaxNamespaceLen = 31

	// MaxBuckets and MaxGrants are how many one namespace may have (ADR 0010); maxListed is the
	// most the portal will list (fail closed above it, so a cap check is never made on a
	// truncated answer).
	MaxBuckets = 3
	MaxGrants  = 10
	maxListed  = 50
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Access is one entry of the fixed menu; the broker maps it to a role in code.
type Access struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// AccessMenu is the order of the select box and the source of truth for the enum.
var AccessMenu = []Access{
	{AccessReader, "can read objects"},
	{AccessWriter, "can read and write objects"},
}

// refused is what Cloud Storage will not accept in a bucket name: "google" anywhere, or a name
// starting with "goog".
func refused(s string) bool { return strings.HasPrefix(s, "goog") || strings.Contains(s, "google") }

// ValidateBucketName checks a claim name against the CRD and the broker: DNS-1123 label, at most
// 18 characters, and none of the names Cloud Storage refuses.
func ValidateBucketName(name string) error {
	if name == "" || len(name) > MaxNameLen || !dnsLabel.MatchString(name) {
		return invalid("name must be a lowercase DNS label of at most %d characters (a-z, 0-9, -)", MaxNameLen)
	}
	if refused(name) {
		return invalid("name cannot contain \"google\" or start with \"goog\"")
	}
	return nil
}

// validateNamespace refuses a namespace the broker cannot name a bucket for.
func validateNamespace(ns string) error {
	if len(ns) > MaxNamespaceLen || refused(ns) {
		return ErrNamespaceUnsupported
	}
	return nil
}

// ValidateAccess checks the menu value.
func ValidateAccess(access string) error {
	for _, a := range AccessMenu {
		if a.Value == access {
			return nil
		}
	}
	return invalid("access must be reader or writer")
}

// GrantName is the deterministic name of a grant, at most 40 + 1 + 18 = 59 characters, so a
// repeated order is a duplicate, not a second grant. (workload a-b on bucket c and workload a on
// bucket b-c share a name; the second order is then refused as a duplicate, never merged.)
func GrantName(workload, bucket string) string { return workload + "-" + bucket }

// Bucket is one BucketClaim as the broker reports it. BucketName stays empty until it has.
type Bucket struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	BucketName string `json:"bucketName"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message,omitempty"`
	CreatedAt  string `json:"createdAt,omitempty"`
}

// BucketDetail is a bucket with its grants.
type BucketDetail struct {
	Bucket
	Grants []Grant `json:"grants"`
}

// Grant is one ResourceGrant.
type Grant struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Workload  string `json:"workload"`
	Bucket    string `json:"bucket"`
	Access    string `json:"access"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
}

// Requester is the authenticated caller, taken from the verified session only.
type Requester = orders.Requester

type Service struct {
	Cluster    Cluster
	Namespaces secrets.Namespaces

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// lock serializes the count-then-create of one namespace in this process, so a burst of orders
// cannot pass the cap check together (single portal replica). The ResourceQuota object counts
// (operator/internal/controller/resources.go) are the atomic backstop in the API server.
func (s *Service) lock(ns string) func() {
	s.mu.Lock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	m := s.locks[ns]
	if m == nil {
		m = &sync.Mutex{}
		s.locks[ns] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func NewService(c Cluster, n secrets.Namespaces) *Service {
	return &Service{Cluster: c, Namespaces: n}
}

// ListNamespaces returns the namespaces the caller may manage resources in: the same ones as for
// secrets and workloads. The page disables those longer than MaxNamespaceLen.
func (s *Service) ListNamespaces(ctx context.Context, who Requester) ([]secrets.Namespace, error) {
	return secrets.ListNamespaces(ctx, s.Namespaces, who)
}

// authorize returns ErrNotFound unless ns is a namespace the caller may manage; a namespace that
// does not exist and one the caller may not touch look the same.
func (s *Service) authorize(ctx context.Context, who Requester, ns string) error {
	_, err := secrets.AuthorizeNamespace(ctx, s.Namespaces, who, ns)
	if errors.Is(err, secrets.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// PlaceBucket creates the BucketClaim and returns it as PENDING; the broker reconciles it.
func (s *Service) PlaceBucket(ctx context.Context, who Requester, ns, name string) (*Bucket, error) {
	if err := s.authorize(ctx, who, ns); err != nil {
		return nil, err
	}
	if err := validateNamespace(ns); err != nil {
		return nil, err
	}
	if err := ValidateBucketName(name); err != nil {
		return nil, err
	}
	defer s.lock(ns)()
	existing, err := s.Cluster.ListBuckets(ctx, ns)
	if err != nil {
		slog.Error("count bucketclaims", "namespace", ns, "err", err)
		return nil, ErrUpstream
	}
	if len(existing) >= MaxBuckets {
		return nil, ErrBucketLimit
	}
	if err := s.Cluster.CreateBucket(ctx, ns, name); err != nil {
		if errors.Is(err, ErrBucketExists) || errors.Is(err, ErrInvalid) {
			return nil, err
		}
		slog.Error("create bucketclaim", "namespace", ns, "name", name, "err", err)
		return nil, ErrUpstream
	}
	return &Bucket{Namespace: ns, Name: name, Status: StatusPending}, nil
}

// ListBuckets returns the buckets of one namespace the caller may manage.
func (s *Service) ListBuckets(ctx context.Context, who Requester, ns string) ([]Bucket, error) {
	if err := s.authorize(ctx, who, ns); err != nil {
		return nil, err
	}
	l, err := s.Cluster.ListBuckets(ctx, ns)
	if err != nil {
		slog.Error("list bucketclaims", "namespace", ns, "err", err)
		return nil, ErrUpstream
	}
	return l, nil
}

// GetBucket returns one bucket and the grants on it.
func (s *Service) GetBucket(ctx context.Context, who Requester, ns, name string) (*BucketDetail, error) {
	if err := s.authorize(ctx, who, ns); err != nil {
		return nil, err
	}
	if ValidateBucketName(name) != nil {
		return nil, ErrNotFound
	}
	b, err := s.Cluster.GetBucket(ctx, ns, name)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		slog.Error("get bucketclaim", "namespace", ns, "name", name, "err", err)
		return nil, ErrUpstream
	}
	all, err := s.Cluster.ListGrants(ctx, ns)
	if err != nil {
		slog.Error("list resourcegrants", "namespace", ns, "err", err)
		return nil, ErrUpstream
	}
	d := &BucketDetail{Bucket: *b, Grants: []Grant{}}
	for _, g := range all {
		if g.Bucket == name {
			d.Grants = append(d.Grants, g)
		}
	}
	return d, nil
}

// PlaceGrant creates the ResourceGrant for (workload, bucket) and returns it as PENDING. The
// workload and the bucket must exist in the namespace; a missing one is a plain not found, in the
// same words as an unknown namespace.
func (s *Service) PlaceGrant(ctx context.Context, who Requester, ns, workload, bucket, access string) (*Grant, error) {
	if err := s.authorize(ctx, who, ns); err != nil {
		return nil, err
	}
	if err := validateNamespace(ns); err != nil {
		return nil, err
	}
	if err := workloads.ValidateName(workload); err != nil {
		return nil, invalid("workload is not a valid workload name")
	}
	if err := ValidateBucketName(bucket); err != nil {
		return nil, err
	}
	if err := ValidateAccess(access); err != nil {
		return nil, err
	}
	defer s.lock(ns)()
	ok, err := s.Cluster.WorkloadExists(ctx, ns, workload)
	if err != nil {
		slog.Error("get workloadidentity", "namespace", ns, "name", workload, "err", err)
		return nil, ErrUpstream
	}
	if !ok {
		return nil, ErrNotFound
	}
	if _, err := s.Cluster.GetBucket(ctx, ns, bucket); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		slog.Error("get bucketclaim", "namespace", ns, "name", bucket, "err", err)
		return nil, ErrUpstream
	}
	existing, err := s.Cluster.ListGrants(ctx, ns)
	if err != nil {
		slog.Error("count resourcegrants", "namespace", ns, "err", err)
		return nil, ErrUpstream
	}
	if len(existing) >= MaxGrants {
		return nil, ErrGrantLimit
	}
	name := GrantName(workload, bucket)
	if err := s.Cluster.CreateGrant(ctx, ns, name, workload, bucket, access); err != nil {
		if errors.Is(err, ErrGrantExists) || errors.Is(err, ErrGrantCollision) || errors.Is(err, ErrInvalid) {
			return nil, err
		}
		slog.Error("create resourcegrant", "namespace", ns, "name", name, "err", err)
		return nil, ErrUpstream
	}
	return &Grant{Namespace: ns, Name: name, Workload: workload, Bucket: bucket, Access: access, Status: StatusPending}, nil
}

// ListGrants returns the grants of one namespace the caller may manage.
func (s *Service) ListGrants(ctx context.Context, who Requester, ns string) ([]Grant, error) {
	if err := s.authorize(ctx, who, ns); err != nil {
		return nil, err
	}
	l, err := s.Cluster.ListGrants(ctx, ns)
	if err != nil {
		slog.Error("list resourcegrants", "namespace", ns, "err", err)
		return nil, ErrUpstream
	}
	return l, nil
}

// RevokeGrant deletes a grant; the broker removes the IAM binding before the object goes. A
// namespace the caller may not touch, a missing grant and a malformed name are all ErrNotFound.
func (s *Service) RevokeGrant(ctx context.Context, who Requester, ns, name string) error {
	if err := s.authorize(ctx, who, ns); err != nil {
		return err
	}
	if len(name) > MaxNameLen+1+40 || !dnsLabel.MatchString(name) {
		return ErrNotFound
	}
	err := s.Cluster.DeleteGrant(ctx, ns, name)
	if errors.Is(err, ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		slog.Error("delete resourcegrant", "namespace", ns, "name", name, "err", err)
		return ErrUpstream
	}
	return nil
}
