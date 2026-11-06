package broker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/storage"
	"github.com/googleapis/gax-go/v2/apierror"
	"golang.org/x/time/rate"
	"google.golang.org/api/googleapi"
)

const (
	gcsCallTimeout = 30 * time.Second
	// Etag conflicts and label races are retried this often, with a short
	// growing pause; the reconciler retries the rest with its own backoff.
	gcsConflictRetries = 6
	// All Google calls share one client-side limit.
	gcsCallsPerSecond = 10
	gcsCallBurst      = 20
)

// GCS is the Cloud implementation on Cloud Storage and the bucket IAM v3
// policy API. It uses application default credentials (on GKE, Workload
// Identity of idp-system/idp-broker) and the Google calls listed in the
// operator README ("Resource broker"). Only plain role/member bindings are
// ever written; conditional bindings are preserved as read.
type GCS struct {
	client        *storage.Client
	projectID     string
	projectNumber string
	location      string
	limiter       *rate.Limiter
	// sleep is replaced in tests.
	sleep func(context.Context, time.Duration) error
}

// NewGCS creates the client. Buckets are created in location; a bucket whose
// project number is not projectNumber is never reported as the broker's.
func NewGCS(ctx context.Context, projectID, projectNumber, location string) (*GCS, error) {
	if projectID == "" || projectNumber == "" || location == "" {
		return nil, errors.New("project id, project number and bucket location are required")
	}
	c, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage client: %w", err)
	}
	return &GCS{client: c, projectID: projectID, projectNumber: projectNumber, location: location,
		limiter: rate.NewLimiter(gcsCallsPerSecond, gcsCallBurst), sleep: sleepCtx}, nil
}

// wait blocks for the client-side rate limit before a Google call.
func (g *GCS) wait(ctx context.Context) error {
	if g.limiter == nil {
		return nil
	}
	return g.limiter.Wait(ctx)
}

// Close releases the client.
func (g *GCS) Close() error { return g.client.Close() }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// httpCode extracts the HTTP status of a Google API error, or 0.
func httpCode(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	var ae *apierror.APIError
	if errors.As(err, &ae) && ae.HTTPCode() > 0 {
		return ae.HTTPCode()
	}
	return 0
}

// isConflict is an etag or metageneration mismatch (412), or an aborted
// concurrent write (409).
func isConflict(err error) bool {
	c := httpCode(err)
	return c == 412 || c == 409
}

// retryConflicts runs fn again, with a growing pause, while it fails with a
// write conflict. fn must re-read what it modifies.
func retryConflicts(ctx context.Context, sleep func(context.Context, time.Duration) error, fn func() error) error {
	var err error
	for i := 0; i < gcsConflictRetries; i++ {
		if err = fn(); err == nil || !isConflict(err) {
			return err
		}
		if serr := sleep(ctx, time.Duration(i+1)*100*time.Millisecond); serr != nil {
			return serr
		}
	}
	return fmt.Errorf("giving up after %d conflicts: %w", gcsConflictRetries, err)
}

// CreateBucket calls storage.buckets.insert. A 409 (the name is taken, by this
// project or another) is ErrAlreadyExists.
func (g *GCS) CreateBucket(ctx context.Context, name string, labels map[string]string) error {
	if err := g.wait(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, gcsCallTimeout)
	defer cancel()
	err := g.client.Bucket(name).Create(ctx, g.projectID, &storage.BucketAttrs{
		Location:                 g.location,
		Labels:                   labels,
		UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true},
		PublicAccessPrevention:   storage.PublicAccessPreventionEnforced,
		VersioningEnabled:        false,
		// No Autoclass, lifecycle, retention, CMEK or logging: the zero values.
	})
	if httpCode(err) == 409 {
		return ErrAlreadyExists
	}
	return err
}

// GetBucket calls storage.buckets.get. Bucket names are global, so a bucket of
// another project (with whatever labels) is ErrForbidden, whether Google
// refuses the read or answers with another project number.
func (g *GCS) GetBucket(ctx context.Context, name string) (*BucketInfo, error) {
	if err := g.wait(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, gcsCallTimeout)
	defer cancel()
	attrs, err := g.client.Bucket(name).Attrs(ctx)
	switch {
	case errors.Is(err, storage.ErrBucketNotExist) || httpCode(err) == 404:
		return &BucketInfo{}, nil
	case httpCode(err) == 403:
		return nil, ErrForbidden
	case err != nil:
		return nil, err
	}
	if strconv.FormatUint(attrs.ProjectNumber, 10) != g.projectNumber {
		return nil, ErrForbidden
	}
	return &BucketInfo{Exists: true, Labels: attrs.Labels, MetaGeneration: attrs.MetaGeneration,
		ProjectNumber: g.projectNumber, Created: attrs.Created}, nil
}

// DeleteBucket calls storage.buckets.delete at the given metageneration,
// which Cloud Storage itself refuses (409) for a bucket with objects. Objects
// are never deleted.
func (g *GCS) DeleteBucket(ctx context.Context, name string, metageneration int64) error {
	if metageneration <= 0 {
		return errors.New("a metageneration is required")
	}
	if err := g.wait(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, gcsCallTimeout)
	defer cancel()
	err := g.client.Bucket(name).If(storage.BucketConditions{MetagenerationMatch: metageneration}).Delete(ctx)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, storage.ErrBucketNotExist) || httpCode(err) == 404:
		return ErrNotFound
	case httpCode(err) == 412:
		return ErrConflict
	case httpCode(err) == 409:
		return ErrNotEmpty
	}
	return err
}

// SetLabels calls storage.buckets.patch (labels only), guarded by the
// metageneration of the read that verified ownership; a moved bucket is
// ErrConflict and the caller re-reads and re-verifies.
func (g *GCS) SetLabels(ctx context.Context, name string, labels map[string]string, metageneration int64) error {
	if metageneration <= 0 {
		return errors.New("a metageneration is required")
	}
	if err := g.wait(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, gcsCallTimeout)
	defer cancel()
	var u storage.BucketAttrsToUpdate
	for k, v := range labels {
		u.SetLabel(k, v)
	}
	_, err := g.client.Bucket(name).If(storage.BucketConditions{MetagenerationMatch: metageneration}).Update(ctx, u)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, storage.ErrBucketNotExist) || httpCode(err) == 404:
		return ErrNotFound
	case httpCode(err) == 412:
		return ErrConflict
	}
	return err
}

// GetPolicy calls storage.buckets.getIamPolicy (policy version 3) and returns
// the unconditional bindings.
func (g *GCS) GetPolicy(ctx context.Context, bucket string) ([]Binding, error) {
	if err := g.wait(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, gcsCallTimeout)
	defer cancel()
	p, err := g.client.Bucket(bucket).IAM().V3().Policy(ctx)
	if err != nil {
		return nil, wrapPolicyErr(err)
	}
	plain, _ := splitBindings(p.Bindings)
	return plain, nil
}

// UpdateBindings reads the policy (storage.buckets.getIamPolicy, version 3,
// with its etag), applies mutate to the unconditional bindings and writes the
// policy back (storage.buckets.setIamPolicy) with the same etag, so a
// concurrent change makes the write fail and the whole read-modify-write is
// retried. Conditional bindings are carried over untouched. An unchanged
// result is not written.
func (g *GCS) UpdateBindings(ctx context.Context, bucket string, scope Scope, mutate func([]Binding) []Binding) error {
	return retryConflicts(ctx, g.sleep, func() error {
		if err := g.wait(ctx); err != nil {
			return err
		}
		cctx, cancel := context.WithTimeout(ctx, gcsCallTimeout)
		defer cancel()
		h := g.client.Bucket(bucket).IAM().V3()
		p, err := h.Policy(cctx)
		if err != nil {
			return wrapPolicyErr(err)
		}
		plain, conditional := splitBindings(p.Bindings)
		next := mutate(cloneBindings(plain))
		if SameBindings(plain, next) {
			return nil
		}
		if err := CheckMutation(scope, plain, next); err != nil {
			return err
		}
		p.Bindings = p.Bindings[:0:0]
		for _, b := range next {
			if len(b.Members) > 0 {
				p.Bindings = append(p.Bindings, &iampb.Binding{Role: b.Role, Members: append([]string(nil), b.Members...)})
			}
		}
		p.Bindings = append(p.Bindings, conditional...)
		return wrapPolicyErr(h.SetPolicy(cctx, p))
	})
}

func wrapPolicyErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, storage.ErrBucketNotExist) || httpCode(err) == 404:
		return ErrNotFound
	}
	return err
}

func splitBindings(in []*iampb.Binding) (plain []Binding, conditional []*iampb.Binding) {
	for _, b := range in {
		if b.GetCondition() != nil {
			conditional = append(conditional, b)
			continue
		}
		plain = append(plain, Binding{Role: b.GetRole(), Members: append([]string(nil), b.GetMembers()...)})
	}
	return plain, conditional
}

func cloneBindings(in []Binding) []Binding {
	out := make([]Binding, len(in))
	for i, b := range in {
		out[i] = Binding{Role: b.Role, Members: append([]string(nil), b.Members...)}
	}
	return out
}

var _ Cloud = (*GCS)(nil)
