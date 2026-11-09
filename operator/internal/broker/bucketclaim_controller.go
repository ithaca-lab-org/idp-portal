package broker

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

// The broker's whole Kubernetes surface (ClusterRole idp-broker, generated
// into config/rbac/broker-role.yaml): read the three kinds it works on plus
// TeamNamespaces and Namespaces, and update only the claims and grants, their
// status and finalizers. No secrets, no other writes. The admission policy
// idp-broker-scope confines those updates further.
//
// +kubebuilder:rbac:groups=idp.ithaca-idp.com,resources=bucketclaims;resourcegrants,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=idp.ithaca-idp.com,resources=bucketclaims/status;resourcegrants/status,verbs=get;update
// +kubebuilder:rbac:groups=idp.ithaca-idp.com,resources=bucketclaims/finalizers;resourcegrants/finalizers,verbs=update
// +kubebuilder:rbac:groups=idp.ithaca-idp.com,resources=workloadidentities;teamnamespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,namespace=idp-system,resources=leases,verbs=get;create;update
// Leader election also records a CORE event (API group "") in idp-system; without this it is rejected (log noise only).
// +kubebuilder:rbac:groups="",namespace=idp-system,resources=events,verbs=create;patch

// adoptSkew is the clock difference tolerated between the apiserver and Cloud
// Storage when checking that a bucket was created after its claim.
const adoptSkew = 2 * time.Minute

// releaseAttempts bounds the re-read and re-verify loop of a release when the
// bucket keeps changing under it.
const releaseAttempts = 4

// BucketClaimReconciler turns a BucketClaim into a bucket. A nil Cloud (broker
// disabled) makes Reconcile a no-op.
type BucketClaimReconciler struct {
	common
}

// NewBucketClaimReconciler wires the reconciler; c is the manager's client.
func NewBucketClaimReconciler(c client.Client, cfg Config) *BucketClaimReconciler {
	return &BucketClaimReconciler{common{Client: c, Config: cfg}}
}

func (r *BucketClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Cloud == nil {
		return ctrl.Result{}, nil
	}
	bc := &idpv1alpha1.BucketClaim{}
	if err := r.Get(ctx, req.NamespacedName, bc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !bc.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(bc, BucketFinalizerName) {
			return r.finalize(ctx, bc)
		}
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx)

	bucket, err := r.bucketName(bc.Namespace, bc.Name)
	if err != nil {
		log.Info("refusing BucketClaim with an invalid name", "reason", err.Error())
		return ctrl.Result{}, r.setNotReady(ctx, bc, ReasonInvalidName, err.Error())
	}
	managed, why, err := r.namespaceManaged(ctx, bc.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !managed {
		log.Info("ignoring BucketClaim outside an operator-managed namespace", "reason", why)
		return ctrl.Result{}, r.setNotReady(ctx, bc, ReasonNamespaceNotManaged, why)
	}

	// The finalizer is in place before the bucket exists, so a delete can never
	// skip the clean-up.
	if controllerutil.AddFinalizer(bc, BucketFinalizerName) {
		if err := r.Update(ctx, bc); err != nil {
			return ctrl.Result{}, err
		}
	}

	outcome, err := r.ensureBucket(ctx, bc, bucket)
	if err != nil {
		log.Error(err, "bucket not provisioned", "bucket", bucket)
		_ = r.setNotReady(ctx, bc, ReasonCreateFailed, "creating or reading the bucket failed; retrying")
		r.event(bc, corev1.EventTypeWarning, ReasonCreateFailed, "creating or reading the bucket failed")
		return ctrl.Result{}, err
	}
	switch outcome {
	case bucketNameTaken:
		log.Info("bucket name is taken; not touching it", "bucket", bucket)
		r.event(bc, corev1.EventTypeWarning, ReasonNameTaken, "a bucket of that name exists that this claim does not own")
		return ctrl.Result{RequeueAfter: nameTakenRetry}, r.setNotReady(ctx, bc, ReasonNameTaken,
			fmt.Sprintf("bucket %s exists and is not owned by this claim (another project, another claim or an earlier incarnation of it, or retired by an earlier deletion); it is left untouched", bucket))
	case bucketRetry:
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if err := setStatus(ctx, r.Client, bc, func() {
		bc.Status.ObservedGeneration = bc.Generation
		bc.Status.BucketName = bucket
		setReady(&bc.Status.Conditions, bc.Generation, metav1.ConditionTrue, ReasonProvisioned,
			fmt.Sprintf("bucket %s exists and is owned by this claim", bucket))
	}); err != nil {
		return ctrl.Result{}, err
	}
	// A slow resync repairs a bucket deleted behind our back.
	return ctrl.Result{RequeueAfter: driftResync}, nil
}

type bucketOutcome int

const (
	bucketReady bucketOutcome = iota
	bucketNameTaken
	bucketRetry
)

// ensureBucket makes sure bucket exists and is this claim's: it creates a
// missing one, adopts one carrying this very claim's labels (same UID: an
// idempotent retry after a crash, never a claim re-created under the same
// name) that was created after the claim, and reports anything else as taken
// without touching it.
func (r *BucketClaimReconciler) ensureBucket(ctx context.Context, bc *idpv1alpha1.BucketClaim, bucket string) (bucketOutcome, error) {
	info, own, err := r.verified(ctx, bucket, bc)
	if err != nil {
		return bucketReady, err
	}
	if !info.Exists {
		err := r.Cloud.CreateBucket(ctx, bucket, OwnerLabels(bc.Namespace, bc.Name, string(bc.UID)))
		switch {
		case err == nil:
			logf.FromContext(ctx).Info("created bucket", "bucket", bucket)
			return bucketReady, nil
		case !errors.Is(err, ErrAlreadyExists):
			return bucketReady, err
		}
		// Someone created it between the read and the create (or it belongs to
		// another project): classify it by project and labels.
		if info, own, err = r.verified(ctx, bucket, bc); err != nil {
			return bucketReady, err
		}
		if !info.Exists {
			return bucketRetry, nil
		}
	}
	if own != Owned || info.Created.Before(bc.CreationTimestamp.Add(-adoptSkew)) {
		return bucketNameTaken, nil
	}
	return bucketReady, nil
}

// finalize releases the bucket of a deleted claim. It never blocks for good:
// every path ends with the finalizer removed (a failing cloud is retried until
// the give-up time, then the bucket is retired best-effort).
func (r *BucketClaimReconciler) finalize(ctx context.Context, bc *idpv1alpha1.BucketClaim) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	orphaned, err := r.release(ctx, bc)
	if err != nil {
		elapsed := r.now().Sub(bc.DeletionTimestamp.Time)
		if elapsed < r.giveUp() {
			log.Error(err, "releasing the bucket failed, will retry")
			if serr := r.setNotReady(ctx, bc, ReasonTerminating,
				fmt.Sprintf("releasing the bucket failed, retrying until %s", bc.DeletionTimestamp.Add(r.giveUp()).UTC().Format(time.RFC3339))); serr != nil {
				return ctrl.Result{}, serr
			}
			return ctrl.Result{RequeueAfter: retryBackoff(elapsed)}, nil
		}
		log.Error(err, "giving up on releasing the bucket; retiring it best-effort, it and its IAM bindings may remain in the project",
			"giveUpAfter", r.giveUp().String())
		r.forceOrphan(ctx, bc)
	}
	if orphaned {
		// Best effort: the object is about to go, the event and the log remain.
		_ = r.setNotReady(ctx, bc, ReasonOrphaned, "the bucket still holds objects: it was kept, labelled idp-orphaned=true, and tenants can no longer reach it")
	}
	controllerutil.RemoveFinalizer(bc, BucketFinalizerName)
	return ctrl.Result{}, r.Update(ctx, bc)
}

// release retires the claim's bucket in an order that no concurrent grant sync
// can undo: (a) label it idp-releasing=true, so every grant refuses it from
// now on; (b) strip the bindings the broker manages; (c) try to delete it
// (Cloud Storage refuses a bucket with objects: then (d) label it orphaned and
// strip the bindings once more, in case a grant wrote between (a) and (b)).
// Every change is guarded by the metageneration of a read that verified
// project, labels and claim UID; a moved bucket is re-read and re-verified.
// Only a bucket carrying exactly this claim's identity is ever changed.
func (r *BucketClaimReconciler) release(ctx context.Context, bc *idpv1alpha1.BucketClaim) (orphaned bool, err error) {
	log := logf.FromContext(ctx)
	bucket, err := r.bucketName(bc.Namespace, bc.Name)
	if err != nil {
		log.Info("invalid claim name; nothing to release", "reason", err.Error())
		return false, nil
	}
	for range releaseAttempts {
		info, own, err := r.verified(ctx, bucket, bc)
		switch {
		case err != nil:
			return false, err
		case !info.Exists || own == Foreign:
			log.Info("bucket is missing or not owned by this claim; leaving it alone", "bucket", bucket)
			return false, nil
		}
		if own == Owned { // (a)
			err := r.Cloud.SetLabels(ctx, bucket, ReleasingLabels(), info.MetaGeneration)
			if errors.Is(err, ErrConflict) {
				continue
			} else if errors.Is(err, ErrNotFound) {
				return false, nil
			} else if err != nil {
				return false, err
			}
		}
		if err := r.stripAll(ctx, bucket, bc.Namespace); err != nil { // (b)
			return false, err
		}
		if own == Orphaned { // retired by an earlier attempt that did not finish
			return true, nil
		}
		// Re-read for the precondition and re-verify (our own label moved it).
		info, own, err = r.verified(ctx, bucket, bc)
		switch {
		case err != nil:
			return false, err
		case !info.Exists || own == Foreign:
			return false, nil
		}
		switch err := r.Cloud.DeleteBucket(ctx, bucket, info.MetaGeneration); { // (c)
		case err == nil:
			log.Info("deleted empty bucket", "bucket", bucket)
			return false, nil
		case errors.Is(err, ErrNotFound):
			return false, nil
		case errors.Is(err, ErrConflict):
			continue
		case !errors.Is(err, ErrNotEmpty):
			return false, err
		}
		// Objects exist: keep the bucket, retire it. // (d)
		err = r.Cloud.SetLabels(ctx, bucket, OrphanLabels(r.now()), info.MetaGeneration)
		if errors.Is(err, ErrConflict) {
			continue
		} else if errors.Is(err, ErrNotFound) {
			return false, nil
		} else if err != nil {
			return false, err
		}
		if err := r.stripAll(ctx, bucket, bc.Namespace); err != nil {
			return false, err
		}
		log.Info("bucket still holds objects; orphaned it (bindings removed, labelled idp-orphaned=true)", "bucket", bucket)
		r.event(bc, corev1.EventTypeWarning, ReasonOrphaned, "bucket holds objects: kept and labelled idp-orphaned=true; delete it by hand when the data is no longer needed")
		return true, nil
	}
	return false, errors.New("the bucket kept changing during release")
}

// forceOrphan is the give-up path: the finalizer is about to be dropped
// although the release failed, so retire the bucket as far as Google answers
// (orphan labels, so no later claim adopts it, and the bindings removed).
// Every step is best effort.
func (r *BucketClaimReconciler) forceOrphan(ctx context.Context, bc *idpv1alpha1.BucketClaim) {
	log := logf.FromContext(ctx)
	bucket, err := r.bucketName(bc.Namespace, bc.Name)
	if err != nil {
		return
	}
	if info, own, err := r.verified(ctx, bucket, bc); err == nil && info.Exists && own != Foreign {
		if err := r.Cloud.SetLabels(ctx, bucket, OrphanLabels(r.now()), info.MetaGeneration); err != nil {
			log.Error(err, "could not label the bucket orphaned", "bucket", bucket)
		}
		if err := r.stripAll(ctx, bucket, bc.Namespace); err != nil {
			log.Error(err, "could not remove the bindings", "bucket", bucket)
		}
	}
}

func (r *BucketClaimReconciler) setNotReady(ctx context.Context, bc *idpv1alpha1.BucketClaim, reason, msg string) error {
	return setStatus(ctx, r.Client, bc, func() {
		bc.Status.ObservedGeneration = bc.Generation
		bc.Status.BucketName = ""
		setReady(&bc.Status.Conditions, bc.Generation, metav1.ConditionFalse, reason, msg)
	})
}

// SetupWithManager watches the claims; Namespace and TeamNamespace events
// re-queue the claims in that namespace, so one created before its namespace
// was managed syncs once it is.
func (r *BucketClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	inNamespace := handler.EnqueueRequestsFromMapFunc(r.requestsForNamespace)
	return ctrl.NewControllerManagedBy(mgr).
		For(&idpv1alpha1.BucketClaim{}).
		Watches(&corev1.Namespace{}, inNamespace).
		Watches(&idpv1alpha1.TeamNamespace{}, inNamespace).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		Named("bucketclaim").
		Complete(r)
}

func (r *BucketClaimReconciler) requestsForNamespace(ctx context.Context, o client.Object) []reconcile.Request {
	list := &idpv1alpha1.BucketClaimList{}
	if err := r.List(ctx, list, client.InNamespace(o.GetName())); err != nil {
		logf.FromContext(ctx).Error(err, "listing BucketClaims", "namespace", o.GetName())
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for _, bc := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: bc.Namespace, Name: bc.Name}})
	}
	return reqs
}
