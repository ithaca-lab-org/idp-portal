package broker

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"
)

// fakeCloud is an in-memory Cloud: buckets with a project, labels, objects,
// a metageneration and an IAM policy with an etag, a log of the calls, and
// failures and hooks that tests inject per operation.
type fakeCloud struct {
	mu      sync.Mutex
	buckets map[string]*fakeBucket
	// calls logs every call as "op bucket" (gets and lists included).
	calls []string
	// failures: op -> errors returned by the next calls of that op, in order.
	failures map[string][]error
	// hooks run once, before the next call of that op executes (outside the
	// lock, so a hook may use the fake or run a reconcile).
	hooks map[string]func()
	// conflicts is the number of UpdateBindings attempts that lose an etag
	// race before one is allowed to succeed.
	conflicts int
	// forbidden names buckets owned by someone else: GetBucket answers
	// ErrForbidden and CreateBucket ErrAlreadyExists.
	forbidden map[string]bool
	// blindGets is the number of GetBucket calls that still see no bucket
	// (a bucket that appears right after the broker looked).
	blindGets int
}

type fakeBucket struct {
	project     string
	labels      map[string]string
	objects     int
	bindings    []Binding
	conditional []Binding // preserved verbatim, never shown to mutate
	etag        int
	writes      int // policy writes
	metagen     int64
	created     time.Time
}

func newFakeCloud() *fakeCloud {
	return &fakeCloud{buckets: map[string]*fakeBucket{}, failures: map[string][]error{}, hooks: map[string]func(){},
		forbidden: map[string]bool{}}
}

func (f *fakeCloud) fail(op string, errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[op] = append(f.failures[op], errs...)
}

// onNext runs fn once, just before the next call of op.
func (f *fakeCloud) onNext(op string, fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hooks[op] = fn
}

func (f *fakeCloud) runHook(op string) {
	f.mu.Lock()
	fn := f.hooks[op]
	delete(f.hooks, op)
	f.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// enter records the call and returns an injected error for it, if any.
func (f *fakeCloud) enter(op, bucket string) error {
	f.calls = append(f.calls, op+" "+bucket)
	if q := f.failures[op]; len(q) > 0 {
		f.failures[op] = q[1:]
		return q[0]
	}
	return nil
}

func (f *fakeCloud) callsTo(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if len(c) > len(op) && c[:len(op)+1] == op+" " {
			n++
		}
	}
	return n
}

// seed adds a bucket in the broker's project made outside the broker.
func (f *fakeCloud) seed(name string, labels map[string]string, objects int) *fakeBucket {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := &fakeBucket{project: testProjectNumber, labels: maps.Clone(labels), objects: objects, etag: 1, metagen: 1, created: time.Now()}
	f.buckets[name] = b
	return b
}

func (f *fakeCloud) bucket(name string) *fakeBucket {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buckets[name]
}

func (f *fakeCloud) CreateBucket(_ context.Context, name string, labels map[string]string) error {
	f.runHook("create")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("create", name); err != nil {
		return err
	}
	if _, ok := f.buckets[name]; ok || f.forbidden[name] {
		return ErrAlreadyExists
	}
	f.buckets[name] = &fakeBucket{project: testProjectNumber, labels: maps.Clone(labels), etag: 1, metagen: 1, created: time.Now()}
	return nil
}

// GetBucket reports the bucket's project, as the controllers must check it
// themselves; ErrForbidden only models a bucket Google refuses to show.
func (f *fakeCloud) GetBucket(_ context.Context, name string) (*BucketInfo, error) {
	f.runHook("get")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("get", name); err != nil {
		return nil, err
	}
	if f.blindGets > 0 {
		f.blindGets--
		return &BucketInfo{}, nil
	}
	if f.forbidden[name] {
		return nil, ErrForbidden
	}
	b, ok := f.buckets[name]
	if !ok {
		return &BucketInfo{}, nil
	}
	return &BucketInfo{Exists: true, Labels: maps.Clone(b.labels), MetaGeneration: b.metagen, ProjectNumber: b.project, Created: b.created}, nil
}

func (f *fakeCloud) DeleteBucket(_ context.Context, name string, metageneration int64) error {
	f.runHook("delete")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("delete", name); err != nil {
		return err
	}
	b, ok := f.buckets[name]
	switch {
	case !ok:
		return ErrNotFound
	case b.metagen != metageneration:
		return ErrConflict
	case b.objects > 0:
		return ErrNotEmpty
	}
	delete(f.buckets, name)
	return nil
}

func (f *fakeCloud) SetLabels(_ context.Context, name string, labels map[string]string, metageneration int64) error {
	f.runHook("labels")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("labels", name); err != nil {
		return err
	}
	b, ok := f.buckets[name]
	switch {
	case !ok:
		return ErrNotFound
	case b.metagen != metageneration:
		return ErrConflict
	}
	maps.Copy(b.labels, labels)
	b.metagen++
	return nil
}

func (f *fakeCloud) GetPolicy(_ context.Context, bucket string) ([]Binding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("policy", bucket); err != nil {
		return nil, err
	}
	b, ok := f.buckets[bucket]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneBindings(b.bindings), nil
}

// UpdateBindings follows the contract of the real one: etag-guarded, retried on
// conflicts, no write for an unchanged result, CheckMutation before a write.
func (f *fakeCloud) UpdateBindings(_ context.Context, bucket string, scope Scope, mutate func([]Binding) []Binding) error {
	f.runHook("update")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("update", bucket); err != nil {
		return err
	}
	b, ok := f.buckets[bucket]
	if !ok {
		return ErrNotFound
	}
	for attempt := 0; attempt < gcsConflictRetries; attempt++ {
		etag := b.etag
		next := mutate(cloneBindings(b.bindings))
		if SameBindings(b.bindings, next) {
			return nil
		}
		if err := CheckMutation(scope, b.bindings, next); err != nil {
			return err
		}
		if f.conflicts > 0 {
			f.conflicts--
			b.etag++ // someone else wrote in between
			continue
		}
		if etag != b.etag {
			continue
		}
		b.bindings = slices.DeleteFunc(next, func(x Binding) bool { return len(x.Members) == 0 })
		b.etag++
		b.writes++
		return nil
	}
	return fmt.Errorf("fake: too many conflicts")
}

var _ Cloud = (*fakeCloud)(nil)

// members returns the members of role on the bucket, sorted.
func (b *fakeBucket) members(role string) []string {
	var out []string
	for _, x := range b.bindings {
		if x.Role == role {
			out = append(out, x.Members...)
		}
	}
	slices.Sort(out)
	return out
}
