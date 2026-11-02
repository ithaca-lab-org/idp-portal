// Package workloads lets a team order a WorkloadIdentity (Order #03) in its own namespaces. The
// WorkloadIdentity resource is the record: the developer chooses only the name, the operator
// derives the ServiceAccount, SPIFFE ID and hostname, and the portal reads them back. Nothing
// is stored in MongoDB and no secret is involved.
package workloads

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ithaca-lab-org/idp-portal/api/internal/orders"
	"github.com/ithaca-lab-org/idp-portal/api/internal/secrets"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid request")
	ErrExists   = errors.New("a workload with this name already exists")
	ErrUpstream = errors.New("the cluster request failed")
	// ErrLimit means the namespace already has MaxPerNamespace workloads.
	ErrLimit = errors.New("this namespace has reached its workload limit")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

const (
	StatusPending  = "PENDING"
	StatusComplete = "COMPLETE"
	StatusFailed   = "FAILED"

	// MaxPerNamespace is how many workloads one namespace may order; maxPerNamespace is the
	// most the portal will list (fail closed above it).
	MaxPerNamespace = 50
	maxPerNamespace = 200
)

// ReservedNames and ReservedPrefixes are the WorkloadIdentity CRD's "name is reserved" rule,
// which is the ManagedSecret rule too (operator/api/v1alpha1/workloadidentity_types.go);
// TestNameRulesMatchTheCRD fails when they drift. The web page gets them from the API.
var (
	ReservedNames    = secrets.ReservedSecretNames
	ReservedPrefixes = secrets.ReservedSecretPrefixes
)

// ValidateName checks a workload name against the CRD rules: DNS-1123 label, at most 40
// characters, not reserved. It is the ManagedSecret name check.
func ValidateName(name string) error {
	if err := secrets.ValidateSecretName(name); err != nil {
		return invalid("%s", strings.TrimPrefix(err.Error(), secrets.ErrInvalid.Error()+": "))
	}
	return nil
}

// Condition is a status condition of the resource, as the operator wrote it.
type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// Workload is one WorkloadIdentity. SpiffeID and Hostname stay empty until the operator
// reports them.
type Workload struct {
	Namespace      string      `json:"namespace"`
	Name           string      `json:"name"`
	ServiceAccount string      `json:"serviceAccount"`
	SpiffeID       string      `json:"spiffeId"`
	Hostname       string      `json:"hostname"`
	Status         string      `json:"status"`
	Reason         string      `json:"reason,omitempty"`
	Conditions     []Condition `json:"conditions"`
	CreatedAt      string      `json:"createdAt,omitempty"`
}

// statusOf maps the operator's Synced condition to the order status: no condition yet is
// PENDING, Synced=True is COMPLETE and Synced=False is FAILED, except PreviousTerminating, which
// clears by itself once the earlier object is gone.
func statusOf(conds []Condition) (status, reason string) {
	for _, c := range conds {
		if c.Type != "Synced" {
			continue
		}
		switch {
		case c.Status == "True":
			return StatusComplete, ""
		case c.Status == "False" && c.Reason != "PreviousTerminating":
			return StatusFailed, c.Reason
		}
		return StatusPending, c.Reason
	}
	return StatusPending, ""
}

// Requester is the authenticated caller, taken from the verified session only.
type Requester = orders.Requester

type Service struct {
	Cluster    Cluster
	Namespaces secrets.Namespaces
}

func NewService(c Cluster, n secrets.Namespaces) *Service {
	return &Service{Cluster: c, Namespaces: n}
}

// ListNamespaces returns the namespaces the caller may order workloads in: the ones they may
// manage secrets in (COMPLETE namespace orders of one of their groups).
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

// Place creates the WorkloadIdentity and returns it as PENDING; the operator reconciles it.
func (s *Service) Place(ctx context.Context, who Requester, ns, name string) (*Workload, error) {
	if err := s.authorize(ctx, who, ns); err != nil {
		return nil, err
	}
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	existing, err := s.Cluster.List(ctx, ns)
	if err != nil {
		slog.Error("count workloadidentities", "namespace", ns, "err", err)
		return nil, ErrUpstream
	}
	if len(existing) >= MaxPerNamespace {
		return nil, ErrLimit
	}
	if err := s.Cluster.Create(ctx, ns, name); err != nil {
		if errors.Is(err, ErrExists) || errors.Is(err, ErrInvalid) {
			return nil, err
		}
		slog.Error("create workloadidentity", "namespace", ns, "name", name, "err", err)
		return nil, ErrUpstream
	}
	return &Workload{Namespace: ns, Name: name, ServiceAccount: name, Status: StatusPending, Conditions: []Condition{}}, nil
}

// Get returns one workload as the operator reports it.
func (s *Service) Get(ctx context.Context, who Requester, ns, name string) (*Workload, error) {
	if err := s.authorize(ctx, who, ns); err != nil {
		return nil, err
	}
	if ValidateName(name) != nil {
		return nil, ErrNotFound
	}
	w, err := s.Cluster.Get(ctx, ns, name)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		slog.Error("get workloadidentity", "namespace", ns, "name", name, "err", err)
		return nil, ErrUpstream
	}
	return w, nil
}

// List returns the workloads of one namespace the caller may manage.
func (s *Service) List(ctx context.Context, who Requester, ns string) ([]Workload, error) {
	if err := s.authorize(ctx, who, ns); err != nil {
		return nil, err
	}
	l, err := s.Cluster.List(ctx, ns)
	if err != nil {
		slog.Error("list workloadidentities", "namespace", ns, "err", err)
		return nil, ErrUpstream
	}
	return l, nil
}
