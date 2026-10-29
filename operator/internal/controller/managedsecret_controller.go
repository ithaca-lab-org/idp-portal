package controller

import (
	"context"
	"fmt"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

// VaultStaticSecret contract (pinned by the admission policy idp-operator-scope;
// change both together).
const (
	VaultStaticSecretMount        = "kv"
	VaultStaticSecretType         = "kv-v2"
	VaultStaticSecretRefreshAfter = "60s"

	LabelManagedSecret = "idp.ithaca-idp.com/managed-secret"

	ReasonSynced              = "Synced"
	ReasonNamespaceNotManaged = "NamespaceNotManaged"
	ReasonVaultAuthMissing    = "VaultAuthMissing"
	ReasonSyncError           = "SyncError"
	ReasonNameConflict        = "NameConflict"
	ReasonPreviousTerminating = "PreviousTerminating"

	ReasonDelivered       = "Delivered"
	ReasonDeliveryFailed  = "DeliveryFailed"
	ReasonDeliveryPending = "DeliveryPending"
	ReasonNotSynced       = "NotSynced"

	// vsoConditionSecretSynced is the VSO 1.6.2 condition type on
	// VaultStaticSecret status (consts.TypeSecretSynced).
	vsoConditionSecretSynced = "SecretSynced"

	vaultAuthMissingRequeue = 15 * time.Second
	previousRequeue         = 5 * time.Second
	nameConflictRequeue     = 5 * time.Minute
)

var VaultStaticSecretGVK = schema.GroupVersionKind{Group: "secrets.hashicorp.com", Version: "v1beta1", Kind: "VaultStaticSecret"}

func newVaultStaticSecret(namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(VaultStaticSecretGVK)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

// desiredVaultStaticSecretSpec is the full spec the operator owns. VSO reads
// kv/data/<namespace>/<name> with the namespace's VaultAuth vault-reader and
// writes the Secret <name>; overwrite=false makes VSO refuse to take over a
// Secret it did not create. Defaulted fields (hmacSecretData, create,
// overwrite) are written explicitly so the stored object compares equal.
func desiredVaultStaticSecretSpec(namespace, name string) map[string]any {
	return map[string]any{
		"vaultAuthRef":   VaultAuthName,
		"mount":          VaultStaticSecretMount,
		"type":           VaultStaticSecretType,
		"path":           namespace + "/" + name,
		"refreshAfter":   VaultStaticSecretRefreshAfter,
		"hmacSecretData": true,
		"destination": map[string]any{
			"name":      name,
			"create":    true,
			"overwrite": false,
		},
	}
}

// ManagedSecretReconciler turns a ManagedSecret into a VaultStaticSecret. It is
// only registered with VAULT_ENABLED=true; a nil Vault makes Reconcile a no-op.
type ManagedSecretReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Vault  *VaultOptions
}

// VaultStaticSecret names follow the ManagedSecrets, so update cannot be
// pinned with resourceNames; the admission policy confines it. No delete:
// garbage collection removes the VaultStaticSecret with its ManagedSecret.
//
// +kubebuilder:rbac:groups=idp.ithaca-idp.com,resources=managedsecrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=idp.ithaca-idp.com,resources=managedsecrets/status,verbs=get;update
// +kubebuilder:rbac:groups=idp.ithaca-idp.com,resources=managedsecrets/finalizers,verbs=update
// +kubebuilder:rbac:groups=secrets.hashicorp.com,resources=vaultstaticsecrets,verbs=get;list;watch;create;update

func (r *ManagedSecretReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Vault == nil {
		return ctrl.Result{}, nil
	}
	ms := &idpv1alpha1.ManagedSecret{}
	if err := r.Get(ctx, req.NamespacedName, ms); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ms.DeletionTimestamp.IsZero() {
		// Owner references do the cleanup (VaultStaticSecret, then VSO's Secret).
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx)

	managed, why, err := r.namespaceManaged(ctx, ms.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !managed {
		log.Info("ignoring ManagedSecret outside an operator-managed namespace", "reason", why)
		return ctrl.Result{}, r.setNotSynced(ctx, ms, ReasonNamespaceNotManaged, why)
	}

	if err := r.Get(ctx, types.NamespacedName{Namespace: ms.Namespace, Name: VaultAuthName}, newVaultAuth("")); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: vaultAuthMissingRequeue}, r.setNotSynced(ctx, ms, ReasonVaultAuthMissing,
			fmt.Sprintf("VaultAuth %s does not exist yet in namespace %s", VaultAuthName, ms.Namespace))
	}

	vss := newVaultStaticSecret("", "")
	err = r.Get(ctx, types.NamespacedName{Namespace: ms.Namespace, Name: ms.Name}, vss)
	switch {
	case apierrors.IsNotFound(err):
		vss = newVaultStaticSecret(ms.Namespace, ms.Name)
		if err := r.setVaultStaticSecret(ms, vss); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, vss); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			_ = r.setNotSynced(ctx, ms, ReasonSyncError, err.Error())
			return ctrl.Result{}, fmt.Errorf("vaultstaticsecret %s: %w", ms.Name, err)
		}
	case err != nil:
		return ctrl.Result{}, err
	default:
		if res, reason, msg := vssBlocked(ms, vss); reason != "" {
			log.Info("not touching existing VaultStaticSecret", "reason", reason)
			return res, r.setNotSynced(ctx, ms, reason, msg)
		}
		before := vss.DeepCopy()
		if err := r.setVaultStaticSecret(ms, vss); err != nil {
			return ctrl.Result{}, err
		}
		if !equality.Semantic.DeepEqual(before.Object, vss.Object) {
			if err := r.Update(ctx, vss); err != nil {
				_ = r.setNotSynced(ctx, ms, ReasonSyncError, err.Error())
				return ctrl.Result{}, fmt.Errorf("vaultstaticsecret %s: %w", ms.Name, err)
			}
		}
	}
	return ctrl.Result{}, r.setConditions(ctx, ms,
		syncedCondition(ms, metav1.ConditionTrue, ReasonSynced,
			fmt.Sprintf("VaultStaticSecret %s for kv/%s/%s exists; see Delivered for the Secret", ms.Name, ms.Namespace, ms.Name)),
		deliveredCondition(ms, vss))
}

// vssBlocked decides about an existing VaultStaticSecret with the
// ManagedSecret's name. Only one controlled by this very ManagedSecret (same
// UID) is updated. Anything else is left alone: one being deleted, or one
// controlled by an earlier ManagedSecret of the same name (the garbage
// collector removes it after a delete and re-create), is waited for; one made
// by hand or by another controller is a name conflict and is never adopted
// (the admission policy refuses that too).
func vssBlocked(ms *idpv1alpha1.ManagedSecret, vss *unstructured.Unstructured) (ctrl.Result, string, string) {
	ref := metav1.GetControllerOf(vss)
	switch {
	case !vss.GetDeletionTimestamp().IsZero():
		return ctrl.Result{RequeueAfter: previousRequeue}, ReasonPreviousTerminating,
			fmt.Sprintf("previous VaultStaticSecret %s is being deleted; waiting", ms.Name)
	case ref != nil && ref.UID == ms.UID:
		return ctrl.Result{}, "", ""
	case ref != nil && ref.Kind == "ManagedSecret" && ref.Name == ms.Name &&
		ref.APIVersion == idpv1alpha1.GroupVersion.String() && vss.GetLabels()[LabelManagedSecret] == ms.Name:
		return ctrl.Result{RequeueAfter: previousRequeue}, ReasonPreviousTerminating,
			fmt.Sprintf("VaultStaticSecret %s still belongs to a deleted ManagedSecret of the same name; waiting for garbage collection", ms.Name)
	}
	return ctrl.Result{RequeueAfter: nameConflictRequeue}, ReasonNameConflict,
		fmt.Sprintf("a VaultStaticSecret named %s exists that this ManagedSecret does not own; it is left untouched", ms.Name)
}

// setVaultStaticSecret writes the operator's labels, spec and controller
// reference into vss.
func (r *ManagedSecretReconciler) setVaultStaticSecret(ms *idpv1alpha1.ManagedSecret, vss *unstructured.Unstructured) error {
	labels := vss.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	maps.Copy(labels, map[string]string{LabelManagedBy: ManagedByValue, LabelManagedSecret: ms.Name})
	vss.SetLabels(labels)
	if want := desiredVaultStaticSecretSpec(ms.Namespace, ms.Name); !equality.Semantic.DeepEqual(vss.Object["spec"], want) {
		vss.Object["spec"] = want
	}
	return controllerutil.SetControllerReference(ms, vss, r.Scheme)
}

// deliveredCondition mirrors VSO's SecretSynced condition on the
// VaultStaticSecret (current generation only). VSO messages are not copied.
// VSO 1.6.2 does not write status when it fails to write the Secret itself
// (for example a foreign Secret of that name with overwrite=false); it only
// records an event, so that case stays Unknown.
func deliveredCondition(ms *idpv1alpha1.ManagedSecret, vss *unstructured.Unstructured) metav1.Condition {
	status, reason := metav1.ConditionUnknown, ReasonDeliveryPending
	msg := fmt.Sprintf("VSO has not reported a sync of VaultStaticSecret %s yet (if this persists, check its events: e.g. a Secret %s that VSO did not create already exists, or kv/%s/%s is missing)",
		ms.Name, ms.Name, ms.Namespace, ms.Name)
	conds, _, _ := unstructured.NestedSlice(vss.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != vsoConditionSecretSynced {
			continue
		}
		if g, _, _ := unstructured.NestedInt64(m, "observedGeneration"); g != vss.GetGeneration() {
			break
		}
		switch m["status"] {
		case string(metav1.ConditionTrue):
			status, reason = metav1.ConditionTrue, ReasonDelivered
			msg = fmt.Sprintf("VSO synced kv/%s/%s into Secret %s", ms.Namespace, ms.Name, ms.Name)
		case string(metav1.ConditionFalse):
			status, reason = metav1.ConditionFalse, ReasonDeliveryFailed
			msg = fmt.Sprintf("VSO failed to sync kv/%s/%s; see VaultStaticSecret %s status and events", ms.Namespace, ms.Name, ms.Name)
		}
	}
	return metav1.Condition{Type: idpv1alpha1.ConditionDelivered, Status: status, Reason: reason, Message: msg, ObservedGeneration: ms.Generation}
}

func syncedCondition(ms *idpv1alpha1.ManagedSecret, status metav1.ConditionStatus, reason, msg string) metav1.Condition {
	return metav1.Condition{Type: idpv1alpha1.ConditionSynced, Status: status, Reason: reason, Message: msg, ObservedGeneration: ms.Generation}
}

// namespaceManaged reports whether ns is a live namespace created by this
// operator for a live TeamNamespace of the same name.
func (r *ManagedSecretReconciler) namespaceManaged(ctx context.Context, name string) (bool, string, error) {
	if IsReserved(name) {
		return false, fmt.Sprintf("namespace %s is reserved for the platform", name), nil
	}
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf("namespace %s does not exist", name), nil
		}
		return false, "", err
	}
	tn := &idpv1alpha1.TeamNamespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, tn); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf("namespace %s has no TeamNamespace", name), nil
		}
		return false, "", err
	}
	switch {
	case !managedBy(ns, tn):
		return false, fmt.Sprintf("namespace %s is not managed by idp-operator", name), nil
	case !ns.DeletionTimestamp.IsZero() || !tn.DeletionTimestamp.IsZero():
		return false, fmt.Sprintf("namespace %s is being deleted", name), nil
	}
	return true, "", nil
}

func (r *ManagedSecretReconciler) setNotSynced(ctx context.Context, ms *idpv1alpha1.ManagedSecret, reason, msg string) error {
	return r.setConditions(ctx, ms, syncedCondition(ms, metav1.ConditionFalse, reason, msg),
		metav1.Condition{Type: idpv1alpha1.ConditionDelivered, Status: metav1.ConditionUnknown, Reason: ReasonNotSynced,
			Message: "no VaultStaticSecret is in place for this ManagedSecret", ObservedGeneration: ms.Generation})
}

func (r *ManagedSecretReconciler) setConditions(ctx context.Context, ms *idpv1alpha1.ManagedSecret, conds ...metav1.Condition) error {
	before := ms.Status.DeepCopy()
	ms.Status.ObservedGeneration = ms.Generation
	for _, c := range conds {
		meta.SetStatusCondition(&ms.Status.Conditions, c)
	}
	if equality.Semantic.DeepEqual(before, &ms.Status) {
		return nil
	}
	return r.Status().Update(ctx, ms)
}

// SetupWithManager watches VaultStaticSecrets (the VSO CRD must exist), so
// call it only with VAULT_ENABLED=true.
func (r *ManagedSecretReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&idpv1alpha1.ManagedSecret{}).
		Owns(newVaultStaticSecret("", "")).
		Named("managedsecret").
		Complete(r)
}
