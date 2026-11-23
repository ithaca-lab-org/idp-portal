// Package pipelines lets a team order a DeliveryPipeline for one of its workloads and ask for a
// deploy of a commit (a DeliveryRun), Order #04 milestone 2 (ADR 0011). The two resources are the
// whole interface: the portal writes them (create only) and the idp-delivery controller, the only
// caller of Spinnaker's Gate, reports phase and message in their status. The portal never talks
// to Gate or Spinnaker and no secret is involved.
package pipelines

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"github.com/ithaca-lab-org/idp-portal/api/internal/orders"
	"github.com/ithaca-lab-org/idp-portal/api/internal/secrets"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid request")
	ErrExists   = errors.New("a pipeline for this workload already exists")
	ErrUpstream = errors.New("the cluster request failed")
	// ErrRunLimit means the namespace already has MaxRuns DeliveryRuns.
	ErrRunLimit = errors.New("this namespace has reached its delivery run limit")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

const (
	DefaultPath = "deploy/base"
	// DefaultAllowedOrgs is the repo owner allowlist when none is configured.
	DefaultAllowedOrgs = "ithaca-lab-org"

	// MaxRuns is how many DeliveryRuns one namespace may hold (the portal cannot delete them).
	MaxRuns = 100
	// ListRuns is how many runs of one pipeline are returned, newest first.
	ListRuns = 20
	// maxListed is the most the portal will list (fail closed above it, so the cap check is never
	// made on a truncated answer).
	maxListed = 500
	maxPipes  = 200

	maxWorkloadLen = 40
)

// The same shapes as the CRDs (operator/api/v1alpha1/delivery*_types.go) and the controller
// (operator/internal/delivery/rules.go); TestRulesMatchTheCRDs fails when they drift.
var (
	labelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	repoRe  = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9._-]{1,100})$`)
	pathRe  = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`)
	shaRe   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// ParseOrgs splits a comma separated allowlist; blanks are dropped and names are lowercased
// (GitHub owners are case insensitive).
func ParseOrgs(s string) []string {
	var out []string
	for _, o := range strings.Split(s, ",") {
		if o = strings.ToLower(strings.TrimSpace(o)); o != "" {
			out = append(out, o)
		}
	}
	return out
}

// ValidateRepo checks the repository URL and its owner against the allowlist.
func ValidateRepo(repo string, allowed []string) error {
	m := repoRe.FindStringSubmatch(repo)
	if len(repo) > 200 || m == nil || m[2] == "." || m[2] == ".." || strings.HasSuffix(m[2], ".git") {
		return invalid("repo must look like https://github.com/<org>/<repo> (no trailing slash, no .git)")
	}
	for _, o := range allowed {
		if strings.EqualFold(o, m[1]) {
			return nil
		}
	}
	return invalid("repository owner %q is not allowed; allowed: %s", m[1], strings.Join(allowed, ", "))
}

// ValidatePath checks the Kustomize directory; empty means DefaultPath, which the caller applies.
func ValidatePath(path string) error {
	if len(path) > 128 || !pathRe.MatchString(path) || strings.Contains(path, "..") ||
		strings.Contains(path, "//") || strings.HasSuffix(path, "/") {
		return invalid("path must be a relative directory without '..', '//' or a trailing '/'")
	}
	return nil
}

// ValidateRef accepts only a full lowercase 40-hex commit SHA.
func ValidateRef(ref string) error {
	if !shaRe.MatchString(ref) {
		return invalid("ref must be a full 40-character lowercase commit SHA; deploy by commit (not a tag, a branch or a short SHA): the image digest is pinned in the repository manifests")
	}
	return nil
}

func validName(n string) bool { return len(n) <= maxWorkloadLen && labelRe.MatchString(n) }

// Pipeline is one DeliveryPipeline as the controller reports it.
type Pipeline struct {
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	Workload        string `json:"workload"`
	Repo            string `json:"repo"`
	Path            string `json:"path"`
	Phase           string `json:"phase"`
	Message         string `json:"message,omitempty"`
	ApplicationName string `json:"applicationName,omitempty"`
	CreatedAt       string `json:"createdAt,omitempty"`
}

// Run is one DeliveryRun.
type Run struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Pipeline    string `json:"pipeline"`
	Ref         string `json:"ref"`
	RequestedBy string `json:"requestedBy,omitempty"`
	Phase       string `json:"phase"`
	Message     string `json:"message,omitempty"`
	ExecutionID string `json:"executionId,omitempty"`
	CreatedAt   string `json:"createdAt,omitempty"`
}

// Requester is the authenticated caller, taken from the verified session only.
type Requester = orders.Requester

type Service struct {
	Cluster     Cluster
	Namespaces  secrets.Namespaces
	AllowedOrgs []string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewService(c Cluster, n secrets.Namespaces, allowedOrgs []string) *Service {
	return &Service{Cluster: c, Namespaces: n, AllowedOrgs: allowedOrgs}
}

// lock serializes the count-then-create of one namespace in this process (single portal replica).
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

// ListNamespaces returns the namespaces the caller may use: the same ones as Secrets and Workloads.
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

// List returns the pipelines of one namespace.
func (s *Service) List(ctx context.Context, who Requester, ns string) ([]Pipeline, error) {
	if err := s.authorize(ctx, who, ns); err != nil {
		return nil, err
	}
	l, err := s.Cluster.ListPipelines(ctx, ns)
	if err != nil {
		slog.Error("list deliverypipelines", "namespace", ns, "err", err)
		return nil, ErrUpstream
	}
	return l, nil
}

// Place creates the DeliveryPipeline of a workload and returns it as PENDING.
func (s *Service) Place(ctx context.Context, who Requester, ns, workload, repo, path string) (*Pipeline, error) {
	if err := s.authorize(ctx, who, ns); err != nil {
		return nil, err
	}
	if !validName(workload) {
		return nil, invalid("workload must be the name of a workload in this namespace")
	}
	if err := ValidateRepo(repo, s.AllowedOrgs); err != nil {
		return nil, err
	}
	if path == "" {
		path = DefaultPath
	}
	if err := ValidatePath(path); err != nil {
		return nil, err
	}
	ok, err := s.Cluster.WorkloadExists(ctx, ns, workload)
	if err != nil {
		slog.Error("get workloadidentity", "namespace", ns, "name", workload, "err", err)
		return nil, ErrUpstream
	}
	if !ok {
		return nil, invalid("workload %q does not exist in this namespace; order it on the Workloads page first", workload)
	}
	p := &Pipeline{Namespace: ns, Name: workload, Workload: workload, Repo: repo, Path: path, Phase: "PENDING"}
	if err := s.Cluster.CreatePipeline(ctx, p); err != nil {
		if errors.Is(err, ErrExists) || errors.Is(err, ErrInvalid) {
			return nil, err
		}
		slog.Error("create deliverypipeline", "namespace", ns, "name", workload, "err", err)
		return nil, ErrUpstream
	}
	slog.Info("audit: deliverypipeline created", "namespace", ns, "pipeline", workload, "repo", repo, "path", path, "sub", who.Sub)
	return p, nil
}

// ListRuns returns the newest runs of one pipeline (at most ListRuns).
func (s *Service) ListRuns(ctx context.Context, who Requester, ns, pipeline string) ([]Run, error) {
	if err := s.authorize(ctx, who, ns); err != nil {
		return nil, err
	}
	if !validName(pipeline) {
		return nil, ErrNotFound
	}
	if _, err := s.Cluster.GetPipeline(ctx, ns, pipeline); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		slog.Error("get deliverypipeline", "namespace", ns, "name", pipeline, "err", err)
		return nil, ErrUpstream
	}
	all, err := s.Cluster.ListRuns(ctx, ns)
	if err != nil {
		slog.Error("list deliveryruns", "namespace", ns, "err", err)
		return nil, ErrUpstream
	}
	out := []Run{}
	for _, r := range all { // the cluster answers newest first
		if r.Pipeline == pipeline && len(out) < ListRuns {
			out = append(out, r)
		}
	}
	return out, nil
}

// PlaceRun creates a DeliveryRun for a commit and returns it as PENDING.
func (s *Service) PlaceRun(ctx context.Context, who Requester, ns, pipeline, ref string) (*Run, error) {
	if err := s.authorize(ctx, who, ns); err != nil {
		return nil, err
	}
	if !validName(pipeline) {
		return nil, ErrNotFound
	}
	if err := ValidateRef(ref); err != nil {
		return nil, err
	}
	defer s.lock(ns)()
	if _, err := s.Cluster.GetPipeline(ctx, ns, pipeline); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		slog.Error("get deliverypipeline", "namespace", ns, "name", pipeline, "err", err)
		return nil, ErrUpstream
	}
	all, err := s.Cluster.ListRuns(ctx, ns)
	if err != nil {
		slog.Error("count deliveryruns", "namespace", ns, "err", err)
		return nil, ErrUpstream
	}
	if len(all) >= MaxRuns {
		return nil, ErrRunLimit
	}
	var run *Run
	for try := 0; try < 3; try++ {
		name, err := RunName(pipeline)
		if err != nil {
			slog.Error("random run name", "err", err)
			return nil, ErrUpstream
		}
		// the subject comes from the session only
		run = &Run{Namespace: ns, Name: name, Pipeline: pipeline, Ref: ref, RequestedBy: who.Sub, Phase: "PENDING"}
		err = s.Cluster.CreateRun(ctx, run)
		if errors.Is(err, ErrExists) {
			continue
		}
		if err != nil {
			if errors.Is(err, ErrInvalid) {
				return nil, err
			}
			slog.Error("create deliveryrun", "namespace", ns, "name", name, "err", err)
			return nil, ErrUpstream
		}
		slog.Info("audit: deliveryrun created", "namespace", ns, "pipeline", pipeline, "run", name, "ref", ref, "sub", who.Sub)
		return run, nil
	}
	return nil, ErrUpstream
}

// RunName is <pipeline>-<8 random hex>: at most 40 + 1 + 8 characters, a DNS label.
func RunName(pipeline string) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return pipeline + "-" + hex.EncodeToString(b), nil
}
