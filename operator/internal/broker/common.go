package broker

import (
	"context"
	"errors"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
	"github.com/ithaca-lab-org/idp-portal/operator/internal/controller"
)

const (
	// Finalizers, pinned by the admission policy idp-broker-scope; change both
	// together. They hold a deleted claim or grant until its cloud side is
	// cleaned up (and never longer than the give-up rules in the README).
	BucketFinalizerName = "idp.ithaca-idp.com/bucket-release"
	GrantFinalizerName  = "idp.ithaca-idp.com/grant-release"

	ReasonProvisioned         = "Provisioned"
	ReasonBound               = "Bound"
	ReasonNamespaceNotManaged = "NamespaceNotManaged"
	ReasonNotManaged          = "NotManaged"
	ReasonInvalidName         = "InvalidName"
	ReasonNameTaken           = "NameTaken"
	ReasonCreateFailed        = "CreateFailed"
	ReasonOrphaned            = "Orphaned"
	ReasonTerminating         = "Terminating"
	ReasonWorkloadNotFound    = "WorkloadNotFound"
	ReasonClaimNotFound       = "ClaimNotFound"
	ReasonClaimNotReady       = "ClaimNotReady"
	ReasonGrantFailed         = "GrantFailed"

	// DefaultGiveUpAfter bounds how long a failing cloud clean-up may hold a
	// finalizer once its namespace is going away (a claim: always).
	DefaultGiveUpAfter = 30 * time.Minute

	waitRequeue     = 15 * time.Second
	nameTakenRetry  = 5 * time.Minute
	driftResync     = 10 * time.Minute
	minRetryBackoff = 10 * time.Second
	maxRetryBackoff = 5 * time.Minute
)

// Config is what both reconcilers need besides the Kubernetes client.
type Config struct {
	// Cloud is nil unless the broker is enabled; Reconcile is then a no-op.
	Cloud         Cloud
	ProjectID     string
	ProjectNumber string
	// GiveUpAfter defaults to DefaultGiveUpAfter.
	GiveUpAfter time.Duration
	// Recorder may be nil (tests).
	Recorder events.EventRecorder
}

// common holds what both reconcilers share.
type common struct {
	client.Client
	Config
	// Now may be nil (time.Now); tests set it.
	Now func() time.Time
	// locks serialise the IAM syncs of one bucket (reconciles run concurrently).
	locks sync.Map
}

// MaxConcurrentReconciles bounds parallel reconciles per controller.
const MaxConcurrentReconciles = 4

func (c *common) lockBucket(bucket string) func() {
	m, _ := c.locks.LoadOrStore(bucket, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (c *common) bucketName(namespace, claim string) (string, error) {
	return BucketName(c.ProjectNumber, namespace, claim)
}

// stripAll removes every binding the broker manages for the namespace from the
// bucket (a missing bucket is fine).
func (c *common) stripAll(ctx context.Context, bucket, namespace string) error {
	scope := c.scope(namespace)
	if err := scope.Validate(); err != nil {
		return err
	}
	err := c.Cloud.UpdateBindings(ctx, bucket, scope, func(cur []Binding) []Binding { return scope.Apply(cur, nil) })
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// verified reads the bucket and classifies it for the claim. ErrForbidden and a
// missing bucket are reported as Foreign with exists=false/true respectively.
func (c *common) verified(ctx context.Context, bucket string, claim client.Object) (*BucketInfo, Ownership, error) {
	info, err := c.Cloud.GetBucket(ctx, bucket)
	switch {
	case errors.Is(err, ErrForbidden):
		return &BucketInfo{Exists: true}, Foreign, nil
	case err != nil:
		return nil, Foreign, err
	case !info.Exists:
		return info, Foreign, nil
	}
	return info, Classify(info, c.ProjectNumber, claim.GetNamespace(), claim.GetName(), string(claim.GetUID())), nil
}

func (c *common) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *common) scope(namespace string) Scope {
	return Scope{ProjectNumber: c.ProjectNumber, ProjectID: c.ProjectID, Namespace: namespace}
}

func (c *common) event(obj runtime.Object, typ, reason, note string) {
	if c.Recorder != nil {
		c.Recorder.Eventf(obj, nil, typ, reason, reason, "%s", note)
	}
}

func (c *common) giveUp() time.Duration {
	if c.GiveUpAfter > 0 {
		return c.GiveUpAfter
	}
	return DefaultGiveUpAfter
}

// retryBackoff grows with the time since deletion: 10 s at first, at most 5 min.
func retryBackoff(elapsed time.Duration) time.Duration {
	return min(max(elapsed/2, minRetryBackoff), maxRetryBackoff)
}

func readyCondition(gen int64, status metav1.ConditionStatus, reason, msg string) metav1.Condition {
	return metav1.Condition{Type: idpv1alpha1.ConditionReady, Status: status, Reason: reason, Message: msg, ObservedGeneration: gen}
}

// setStatus applies mutate to obj's status and writes it only if the object
// changed (it compares against a deep copy, so in-place condition updates count).
func setStatus[T client.Object](ctx context.Context, c client.Client, obj T, mutate func()) error {
	before := obj.DeepCopyObject()
	mutate()
	if equality.Semantic.DeepEqual(before, obj) {
		return nil
	}
	return c.Status().Update(ctx, obj)
}

func setReady(conds *[]metav1.Condition, gen int64, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(conds, readyCondition(gen, status, reason, msg))
}

// workloadExists reports whether a live WorkloadIdentity of that name exists.
// It is a read-only Get.
func (c *common) workloadExists(ctx context.Context, namespace, name string) (bool, error) {
	wi := &idpv1alpha1.WorkloadIdentity{}
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, wi)
	switch {
	case client.IgnoreNotFound(err) != nil:
		return false, err
	case err != nil:
		return false, nil
	}
	return wi.DeletionTimestamp.IsZero(), nil
}

func (c *common) namespaceManaged(ctx context.Context, namespace string) (bool, string, error) {
	return controller.NamespaceManaged(ctx, c.Client, namespace)
}
