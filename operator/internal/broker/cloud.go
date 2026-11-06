// Package broker is the resource broker of ADR 0010: it turns BucketClaims
// into Cloud Storage buckets and ResourceGrants into bucket-level IAM
// bindings. It runs as its own Deployment (cmd/broker), never inside the
// operator manager, and reaches Google only through the Cloud interface.
package broker

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrAlreadyExists is returned by CreateBucket when the name is taken, by
	// this project or by anyone else (bucket names are global).
	ErrAlreadyExists = errors.New("bucket already exists")
	// ErrNotFound is returned when the bucket does not exist.
	ErrNotFound = errors.New("bucket not found")
	// ErrNotEmpty is returned by DeleteBucket for a bucket that still holds
	// objects. The broker never deletes objects.
	ErrNotEmpty = errors.New("bucket is not empty")
	// ErrForbidden is returned by GetBucket for a bucket the broker may not
	// read or that belongs to another project (bucket names are global).
	ErrForbidden = errors.New("bucket is not accessible to the broker")
	// ErrConflict is returned by DeleteBucket and SetLabels when the
	// metageneration precondition fails: the bucket changed since it was read.
	ErrConflict = errors.New("bucket changed since it was read")
)

// BucketInfo is what the broker needs to know about an existing bucket.
type BucketInfo struct {
	Exists bool
	Labels map[string]string
	// MetaGeneration is the precondition for DeleteBucket and SetLabels.
	MetaGeneration int64
	// ProjectNumber is the project that owns the bucket; the controllers
	// refuse any bucket of another project.
	ProjectNumber string
	// Created is the bucket creation time.
	Created time.Time
}

// Binding is a plain IAM role/member binding. Conditional bindings are never
// shown to the broker (see UpdateBindings).
type Binding struct {
	Role    string
	Members []string
}

// Cloud is the only way the controllers reach Google Cloud. The real
// implementation is GCS (gcs.go); tests use an in-memory fake.
type Cloud interface {
	// CreateBucket creates the bucket with the labels and the platform
	// settings (uniform bucket-level access, public access prevention
	// enforced, no versioning, no autoclass, the configured location). It
	// returns ErrAlreadyExists when the name is taken.
	CreateBucket(ctx context.Context, name string, labels map[string]string) error
	// GetBucket returns Exists=false (and no error) for a missing bucket and
	// ErrForbidden for one the broker may not read or that is not in the
	// broker's project.
	GetBucket(ctx context.Context, name string) (*BucketInfo, error)
	// DeleteBucket deletes an empty bucket only: never recursive, ErrNotEmpty
	// when objects exist, ErrConflict when the metageneration moved.
	DeleteBucket(ctx context.Context, name string, metageneration int64) error
	// SetLabels adds or overwrites the given labels and keeps all others,
	// only if the bucket is still at metageneration (else ErrConflict).
	SetLabels(ctx context.Context, name string, labels map[string]string, metageneration int64) error
	// GetPolicy returns the plain (unconditional) bindings of the bucket.
	GetPolicy(ctx context.Context, bucket string) ([]Binding, error)
	// UpdateBindings is a read-modify-write of the bucket's IAM policy: it
	// reads the policy with its etag, calls mutate with the plain bindings,
	// checks the difference with CheckMutation(scope, ...), and writes the
	// result back only if the etag still matches, retrying on a conflict.
	// Conditional bindings are kept untouched and never passed to mutate. When
	// mutate returns the bindings unchanged nothing is written.
	UpdateBindings(ctx context.Context, bucket string, scope Scope, mutate func([]Binding) []Binding) error
}
