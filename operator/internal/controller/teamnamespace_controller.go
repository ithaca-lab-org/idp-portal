package controller

import (
	"context"
	"fmt"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

const (
	ReasonReconciled           = "Reconciled"
	ReasonReservedName         = "ReservedName"
	ReasonNamespaceConflict    = "NamespaceConflict"
	ReasonNamespaceTerminating = "NamespaceTerminating"
	ReasonReconcileError       = "ReconcileError"
)

type TeamNamespaceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// The child objects live in namespaces created at runtime, so their rules must be
// cluster-wide; update/delete are pinned to the deterministic child names.
// The operator gets "bind" on the admin ClusterRole only (no "escalate"), which is
// what the API server requires to create RoleBindings that reference it.
//
// +kubebuilder:rbac:groups=idp.ithaca-idp.com,resources=teamnamespaces,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=idp.ithaca-idp.com,resources=teamnamespaces/status,verbs=get;update
// +kubebuilder:rbac:groups=idp.ithaca-idp.com,resources=teamnamespaces/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=resourcequotas;limitranges,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=update,resourceNames=idp-quota
// +kubebuilder:rbac:groups="",resources=limitranges,verbs=update,resourceNames=idp-limits
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=update,resourceNames=idp-default-deny-ingress;idp-allow-same-namespace
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=update;delete,resourceNames=idp-namespace-admin
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=bind,resourceNames=admin

func (r *TeamNamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	tn := &idpv1alpha1.TeamNamespace{}
	if err := r.Get(ctx, req.NamespacedName, tn); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !tn.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, tn)
	}

	if IsReserved(tn.Name) {
		log.Info("refusing reserved namespace name")
		return ctrl.Result{}, r.setStatus(ctx, tn, idpv1alpha1.PhaseFailed, metav1.ConditionFalse, ReasonReservedName,
			fmt.Sprintf("namespace name %q is reserved for the platform", tn.Name), "")
	}

	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: tn.Name}, ns); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		ns = nil
	}
	if ns != nil && !managedBy(ns, tn) {
		log.Info("refusing to adopt unmanaged namespace")
		return ctrl.Result{}, r.setStatus(ctx, tn, idpv1alpha1.PhaseFailed, metav1.ConditionFalse, ReasonNamespaceConflict,
			fmt.Sprintf("namespace %q already exists and is not managed by this TeamNamespace", tn.Name), "")
	}
	if ns != nil && !ns.DeletionTimestamp.IsZero() {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, r.setStatus(ctx, tn, idpv1alpha1.PhasePending, metav1.ConditionFalse,
			ReasonNamespaceTerminating, "previous namespace is still terminating", "")
	}

	if controllerutil.AddFinalizer(tn, FinalizerName) {
		if err := r.Update(ctx, tn); err != nil {
			return ctrl.Result{}, err
		}
	}

	requeue, err := r.converge(ctx, tn, ns)
	if err != nil {
		_ = r.setStatus(ctx, tn, idpv1alpha1.PhasePending, metav1.ConditionFalse, ReasonReconcileError, err.Error(), tn.Status.Namespace)
		return ctrl.Result{}, err
	}
	if requeue {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return ctrl.Result{}, r.setStatus(ctx, tn, idpv1alpha1.PhaseReady, metav1.ConditionTrue, ReasonReconciled,
		"namespace and guardrails are in place", tn.Name)
}

func managedBy(ns *corev1.Namespace, tn *idpv1alpha1.TeamNamespace) bool {
	return ns.Labels[LabelManagedBy] == ManagedByValue && ns.Labels[LabelTeamNamespace] == tn.Name
}

func (r *TeamNamespaceReconciler) converge(ctx context.Context, tn *idpv1alpha1.TeamNamespace, ns *corev1.Namespace) (bool, error) {
	if err := r.ensureNamespace(ctx, tn, ns); err != nil {
		return false, fmt.Errorf("namespace: %w", err)
	}

	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: QuotaName, Namespace: tn.Name}}
	if err := r.ensureChild(ctx, tn, quota, func() {
		if want := desiredQuotaSpec(tn); !equality.Semantic.DeepEqual(quota.Spec, want) {
			quota.Spec = want
		}
	}); err != nil {
		return false, fmt.Errorf("resourcequota: %w", err)
	}

	lr := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: LimitRangeName, Namespace: tn.Name}}
	if err := r.ensureChild(ctx, tn, lr, func() {
		if want := desiredLimitRangeSpec(tn); !equality.Semantic.DeepEqual(lr.Spec, want) {
			lr.Spec = want
		}
	}); err != nil {
		return false, fmt.Errorf("limitrange: %w", err)
	}

	for name, want := range map[string]networkingv1.NetworkPolicySpec{
		DenyIngressPolicyName: desiredDenyIngressSpec(),
		SameNamespacePolicy:   desiredSameNamespaceSpec(),
	} {
		np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tn.Name}}
		if err := r.ensureChild(ctx, tn, np, func() {
			if !equality.Semantic.DeepEqual(np.Spec, want) {
				np.Spec = want
			}
		}); err != nil {
			return false, fmt.Errorf("networkpolicy %s: %w", name, err)
		}
	}

	return r.ensureRoleBinding(ctx, tn)
}

func (r *TeamNamespaceReconciler) ensureNamespace(ctx context.Context, tn *idpv1alpha1.TeamNamespace, ns *corev1.Namespace) error {
	if ns == nil {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tn.Name}}
		applyNamespaceMeta(ns, tn)
		return r.Create(ctx, ns)
	}
	before := ns.DeepCopy()
	applyNamespaceMeta(ns, tn)
	if equality.Semantic.DeepEqual(before.ObjectMeta, ns.ObjectMeta) {
		return nil
	}
	// Update (not Patch) so the resourceVersion of the object we checked for our
	// labels is enforced; a concurrent relabel makes this fail instead of re-adopting.
	return r.Update(ctx, ns)
}

func applyNamespaceMeta(ns *corev1.Namespace, tn *idpv1alpha1.TeamNamespace) {
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	maps.Copy(ns.Labels, namespaceLabels(tn))
	if ns.Annotations == nil {
		ns.Annotations = map[string]string{}
	}
	ns.Annotations[AnnotationOwner] = tn.Spec.Owner
	if tn.Spec.OrderID != "" {
		ns.Annotations[AnnotationOrderID] = tn.Spec.OrderID
	} else {
		delete(ns.Annotations, AnnotationOrderID)
	}
}

func (r *TeamNamespaceReconciler) ensureChild(ctx context.Context, tn *idpv1alpha1.TeamNamespace, obj client.Object, setSpec func()) error {
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		maps.Copy(labels, childLabels(tn))
		obj.SetLabels(labels)
		setSpec()
		return controllerutil.SetControllerReference(tn, obj, r.Scheme)
	})
	return err
}

// ensureRoleBinding reports true when it had to delete a binding whose immutable
// roleRef drifted; the replacement is created on the next pass.
func (r *TeamNamespaceReconciler) ensureRoleBinding(ctx context.Context, tn *idpv1alpha1.TeamNamespace) (bool, error) {
	rb := &rbacv1.RoleBinding{}
	err := r.Get(ctx, types.NamespacedName{Name: AdminRoleBindingName, Namespace: tn.Name}, rb)
	switch {
	case err == nil && rb.RoleRef != desiredRoleRef():
		uid := rb.UID
		if err := r.Delete(ctx, rb, client.Preconditions{UID: &uid}); client.IgnoreNotFound(err) != nil {
			return false, fmt.Errorf("rolebinding: %w", err)
		}
		return true, nil
	case err != nil && !apierrors.IsNotFound(err):
		return false, fmt.Errorf("rolebinding: %w", err)
	}

	rb = &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: AdminRoleBindingName, Namespace: tn.Name}}
	err = r.ensureChild(ctx, tn, rb, func() {
		rb.RoleRef = desiredRoleRef()
		if want := desiredSubjects(tn); !equality.Semantic.DeepEqual(rb.Subjects, want) {
			rb.Subjects = want
		}
	})
	if err != nil {
		return false, fmt.Errorf("rolebinding: %w", err)
	}
	return false, nil
}

func (r *TeamNamespaceReconciler) finalize(ctx context.Context, tn *idpv1alpha1.TeamNamespace) error {
	if !controllerutil.ContainsFinalizer(tn, FinalizerName) {
		return nil
	}
	if !IsReserved(tn.Name) {
		ns := &corev1.Namespace{}
		err := r.Get(ctx, types.NamespacedName{Name: tn.Name}, ns)
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return err
		case managedBy(ns, tn) && ns.DeletionTimestamp.IsZero():
			// Preconditions pin the exact object whose labels were checked.
			uid, rv := ns.UID, ns.ResourceVersion
			if err := r.Delete(ctx, ns, client.Preconditions{UID: &uid, ResourceVersion: &rv}); client.IgnoreNotFound(err) != nil {
				return err
			}
			logf.FromContext(ctx).Info("deleted managed namespace")
		}
	}
	controllerutil.RemoveFinalizer(tn, FinalizerName)
	return r.Update(ctx, tn)
}

func (r *TeamNamespaceReconciler) setStatus(ctx context.Context, tn *idpv1alpha1.TeamNamespace, phase idpv1alpha1.Phase,
	cond metav1.ConditionStatus, reason, msg, namespace string) error {
	before := tn.Status.DeepCopy()
	tn.Status.Phase = phase
	tn.Status.ObservedGeneration = tn.Generation
	tn.Status.Namespace = namespace
	meta.SetStatusCondition(&tn.Status.Conditions, metav1.Condition{
		Type:               idpv1alpha1.ConditionReady,
		Status:             cond,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: tn.Generation,
	})
	if equality.Semantic.DeepEqual(before, &tn.Status) {
		return nil
	}
	return r.Status().Update(ctx, tn)
}

func (r *TeamNamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&idpv1alpha1.TeamNamespace{}).
		Owns(&corev1.ResourceQuota{}).
		Owns(&corev1.LimitRange{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&rbacv1.RoleBinding{}).
		// Namespace events map by name so a conflict clears once the foreign
		// namespace is gone, and a managed namespace is re-converged on drift.
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, o client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: o.GetName()}}}
			})).
		Named("teamnamespace").
		Complete(r)
}
