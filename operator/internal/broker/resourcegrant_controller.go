package broker

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

var (
	// These stop a binding sync before any IAM write: there is nothing to bind
	// to, the claim is missing or being deleted, or the bucket is not this
	// claim's (another project, other labels or UID) or is already being
	// released or retired.
	errBucketMissing = errors.New("the claim's bucket does not exist")
	errBucketNotOurs = errors.New("the bucket is not owned by this claim or is being released")
	errClaimGone     = errors.New("the BucketClaim is missing or being deleted")
)

// ResourceGrantReconciler keeps the IAM bindings of a claim's bucket equal to
// the ResourceGrants that name it. A nil Cloud (broker disabled) makes
// Reconcile a no-op.
type ResourceGrantReconciler struct {
	common
}

// NewResourceGrantReconciler wires the reconciler; c is the manager's client.
func NewResourceGrantReconciler(c client.Client, cfg Config) *ResourceGrantReconciler {
	return &ResourceGrantReconciler{common{Client: c, Config: cfg}}
}

func (r *ResourceGrantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Cloud == nil {
		return ctrl.Result{}, nil
	}
	rg := &idpv1alpha1.ResourceGrant{}
	if err := r.Get(ctx, req.NamespacedName, rg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !rg.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(rg, GrantFinalizerName) {
			return r.finalize(ctx, rg)
		}
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx)

	managed, why, err := r.namespaceManaged(ctx, rg.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !managed {
		log.Info("ignoring ResourceGrant outside an operator-managed namespace", "reason", why)
		return ctrl.Result{}, r.setNotReady(ctx, rg, "", ReasonNotManaged, why)
	}
	scope := r.scope(rg.Namespace)
	member, err := scope.Member(rg.Spec.Workload)
	if err != nil {
		return ctrl.Result{}, r.setNotReady(ctx, rg, "", ReasonGrantFailed, err.Error())
	}
	if _, ok := RoleFor(rg.Spec.Access); !ok {
		return ctrl.Result{}, r.setNotReady(ctx, rg, member, ReasonGrantFailed, fmt.Sprintf("access %q is not in the menu", rg.Spec.Access))
	}
	bucket, err := r.bucketName(rg.Namespace, rg.Spec.Resource)
	if err != nil {
		return ctrl.Result{}, r.setNotReady(ctx, rg, member, ReasonClaimNotFound, err.Error())
	}

	claim := &idpv1alpha1.BucketClaim{}
	switch err := r.Get(ctx, types.NamespacedName{Namespace: rg.Namespace, Name: rg.Spec.Resource}, claim); {
	case apierrors.IsNotFound(err):
		return ctrl.Result{RequeueAfter: waitRequeue}, r.setNotReady(ctx, rg, member, ReasonClaimNotFound,
			fmt.Sprintf("BucketClaim %s does not exist in namespace %s", rg.Spec.Resource, rg.Namespace))
	case err != nil:
		return ctrl.Result{}, err
	case !claim.DeletionTimestamp.IsZero() || !meta.IsStatusConditionTrue(claim.Status.Conditions, idpv1alpha1.ConditionReady) ||
		claim.Status.BucketName != bucket:
		return ctrl.Result{RequeueAfter: waitRequeue}, r.setNotReady(ctx, rg, member, ReasonClaimNotReady,
			fmt.Sprintf("BucketClaim %s is not Ready", rg.Spec.Resource))
	}

	// The finalizer is in place before the binding exists.
	if controllerutil.AddFinalizer(rg, GrantFinalizerName) {
		if err := r.Update(ctx, rg); err != nil {
			return ctrl.Result{}, err
		}
	}

	// The bucket's whole managed binding set is recomputed from all grants, so
	// a missing workload also revokes what an earlier reconcile granted.
	err = r.syncBindings(ctx, rg.Namespace, rg.Spec.Resource, bucket)
	switch {
	case isNothingToBind(err):
		return ctrl.Result{RequeueAfter: waitRequeue}, r.setNotReady(ctx, rg, member, ReasonClaimNotReady, err.Error())
	case err != nil:
		log.Error(err, "binding failed", "bucket", bucket)
		_ = r.setNotReady(ctx, rg, member, ReasonGrantFailed, "updating the bucket's IAM policy failed; retrying")
		r.event(rg, corev1.EventTypeWarning, ReasonGrantFailed, "updating the bucket's IAM policy failed")
		return ctrl.Result{}, err
	}
	exists, err := r.workloadExists(ctx, rg.Namespace, rg.Spec.Workload)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !exists {
		return ctrl.Result{RequeueAfter: waitRequeue}, r.setNotReady(ctx, rg, member, ReasonWorkloadNotFound,
			fmt.Sprintf("WorkloadIdentity %s does not exist in namespace %s; no access is bound", rg.Spec.Workload, rg.Namespace))
	}
	if err := setStatus(ctx, r.Client, rg, func() {
		rg.Status.ObservedGeneration = rg.Generation
		rg.Status.Member = member
		setReady(&rg.Status.Conditions, rg.Generation, metav1.ConditionTrue, ReasonBound,
			fmt.Sprintf("%s access to bucket %s is bound for workload %s (IAM may take a few minutes to propagate)", rg.Spec.Access, bucket, rg.Spec.Workload))
	}); err != nil {
		return ctrl.Result{}, err
	}
	// A slow resync repairs IAM drift made behind our back.
	return ctrl.Result{RequeueAfter: driftResync}, nil
}

func isNothingToBind(err error) bool {
	return errors.Is(err, errBucketMissing) || errors.Is(err, errBucketNotOurs) || errors.Is(err, errClaimGone)
}

// syncBindings makes the broker-managed bindings of the claim's bucket equal to
// the live ResourceGrants of that claim: those not being deleted whose
// WorkloadIdentity exists. One read-modify-write; every other binding is left
// as it is. It writes nothing when the claim is missing or deleting, or the
// bucket is missing, of another project, not this claim's, or being released
// or retired. Syncs of one bucket are serialised; after the write the bucket is
// read again and, if a release began meanwhile, the bindings are stripped
// again, so a write that raced a release never survives it.
func (r *ResourceGrantReconciler) syncBindings(ctx context.Context, namespace, claimName, bucket string) error {
	claim := &idpv1alpha1.BucketClaim{}
	switch err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: claimName}, claim); {
	case apierrors.IsNotFound(err):
		return errClaimGone
	case err != nil:
		return err
	case !claim.DeletionTimestamp.IsZero():
		return errClaimGone
	}
	defer r.lockBucket(bucket)()

	info, own, err := r.verified(ctx, bucket, claim)
	switch {
	case err != nil:
		return err
	case !info.Exists:
		return errBucketMissing
	case own != Owned:
		return errBucketNotOurs
	}

	grants, err := r.liveGrants(ctx, namespace, claimName)
	if err != nil {
		return err
	}
	scope := r.scope(namespace)
	if err := scope.Validate(); err != nil {
		return err
	}
	err = r.Cloud.UpdateBindings(ctx, bucket, scope, func(cur []Binding) []Binding { return scope.Apply(cur, grants) })
	if errors.Is(err, ErrNotFound) {
		return errBucketMissing
	} else if err != nil {
		return err
	}
	if info, own, err = r.verified(ctx, bucket, claim); err != nil {
		return err
	} else if info.Exists && own != Owned {
		if err := r.stripAll(ctx, bucket, namespace); err != nil {
			return err
		}
		return errBucketNotOurs
	}
	return nil
}

// liveGrants lists the grants of the claim that currently deserve a binding.
func (r *ResourceGrantReconciler) liveGrants(ctx context.Context, namespace, claim string) ([]Grant, error) {
	list := &idpv1alpha1.ResourceGrantList{}
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	workloads := map[string]bool{}
	var grants []Grant
	for _, g := range list.Items {
		if g.Spec.Resource != claim || !g.DeletionTimestamp.IsZero() {
			continue
		}
		ok, seen := workloads[g.Spec.Workload]
		if !seen {
			var err error
			if ok, err = r.workloadExists(ctx, namespace, g.Spec.Workload); err != nil {
				return nil, err
			}
			workloads[g.Spec.Workload] = ok
		}
		if ok {
			grants = append(grants, Grant{Workload: g.Spec.Workload, Access: g.Spec.Access})
		}
	}
	return grants, nil
}

// finalize recomputes the bucket's bindings without the deleted grant and then
// drops the finalizer. A failing cloud is retried; only when the namespace is
// itself going away (or gone) does the give-up time apply, so a cluster
// teardown never hangs on it while a live namespace never keeps a revoked grant.
func (r *ResourceGrantReconciler) finalize(ctx context.Context, rg *idpv1alpha1.ResourceGrant) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var err error
	if bucket, nerr := r.bucketName(rg.Namespace, rg.Spec.Resource); nerr != nil {
		log.Info("invalid claim name; nothing to release", "reason", nerr.Error())
	} else {
		err = r.syncBindings(ctx, rg.Namespace, rg.Spec.Resource, bucket)
	}
	if isNothingToBind(err) {
		// Releasing, retired or gone: the claim's release removes every binding.
		err = nil
	}
	if err != nil {
		elapsed := r.now().Sub(rg.DeletionTimestamp.Time)
		managed, _, merr := r.namespaceManaged(ctx, rg.Namespace)
		if merr != nil || managed || elapsed < r.giveUp() {
			log.Error(err, "removing the grant's binding failed, will retry")
			if serr := r.setNotReady(ctx, rg, rg.Status.Member, ReasonTerminating, "removing the binding failed; retrying"); serr != nil {
				return ctrl.Result{}, serr
			}
			return ctrl.Result{RequeueAfter: retryBackoff(elapsed)}, nil
		}
		log.Error(err, "giving up on removing the binding of a grant in a deleted namespace; it may remain on the bucket",
			"giveUpAfter", r.giveUp().String())
	}
	controllerutil.RemoveFinalizer(rg, GrantFinalizerName)
	return ctrl.Result{}, r.Update(ctx, rg)
}

func (r *ResourceGrantReconciler) setNotReady(ctx context.Context, rg *idpv1alpha1.ResourceGrant, member, reason, msg string) error {
	return setStatus(ctx, r.Client, rg, func() {
		rg.Status.ObservedGeneration = rg.Generation
		rg.Status.Member = member
		setReady(&rg.Status.Conditions, rg.Generation, metav1.ConditionFalse, reason, msg)
	})
}

// SetupWithManager watches the grants, and re-queues them on events of what
// they depend on: the claim and WorkloadIdentity they name, and their
// Namespace and TeamNamespace.
func (r *ResourceGrantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&idpv1alpha1.ResourceGrant{}).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.grantsWhere(func(idpv1alpha1.ResourceGrant, client.Object) bool { return true }))).
		Watches(&idpv1alpha1.TeamNamespace{}, handler.EnqueueRequestsFromMapFunc(r.grantsWhere(func(idpv1alpha1.ResourceGrant, client.Object) bool { return true }))).
		Watches(&idpv1alpha1.BucketClaim{}, handler.EnqueueRequestsFromMapFunc(r.grantsWhere(
			func(g idpv1alpha1.ResourceGrant, o client.Object) bool { return g.Spec.Resource == o.GetName() }))).
		Watches(&idpv1alpha1.WorkloadIdentity{}, handler.EnqueueRequestsFromMapFunc(r.grantsWhere(
			func(g idpv1alpha1.ResourceGrant, o client.Object) bool { return g.Spec.Workload == o.GetName() }))).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		Named("resourcegrant").
		Complete(r)
}

// grantsWhere maps an object to the ResourceGrants for which match holds: those
// in its namespace (a Namespace or TeamNamespace is named after the namespace
// and has no namespace of its own).
func (r *ResourceGrantReconciler) grantsWhere(match func(idpv1alpha1.ResourceGrant, client.Object) bool) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		ns := o.GetNamespace()
		if ns == "" {
			ns = o.GetName()
		}
		list := &idpv1alpha1.ResourceGrantList{}
		if err := r.List(ctx, list, client.InNamespace(ns)); err != nil {
			logf.FromContext(ctx).Error(err, "listing ResourceGrants", "namespace", ns)
			return nil
		}
		var reqs []reconcile.Request
		for _, g := range list.Items {
			if match(g, o) {
				reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: g.Namespace, Name: g.Name}})
			}
		}
		return reqs
	}
}
