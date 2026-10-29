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

	vaultAuthMissingRequeue = 15 * time.Second
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
		return ctrl.Result{}, r.setSynced(ctx, ms, metav1.ConditionFalse, ReasonNamespaceNotManaged, why)
	}

	if err := r.Get(ctx, types.NamespacedName{Namespace: ms.Namespace, Name: VaultAuthName}, newVaultAuth("")); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: vaultAuthMissingRequeue}, r.setSynced(ctx, ms, metav1.ConditionFalse, ReasonVaultAuthMissing,
			fmt.Sprintf("VaultAuth %s does not exist yet in namespace %s", VaultAuthName, ms.Namespace))
	}

	vss := newVaultStaticSecret(ms.Namespace, ms.Name)
	want := desiredVaultStaticSecretSpec(ms.Namespace, ms.Name)
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, vss, func() error {
		labels := vss.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		maps.Copy(labels, map[string]string{LabelManagedBy: ManagedByValue, LabelManagedSecret: ms.Name})
		vss.SetLabels(labels)
		if cur, ok := vss.Object["spec"].(map[string]any); !ok || !equality.Semantic.DeepEqual(cur, want) {
			vss.Object["spec"] = want
		}
		return controllerutil.SetControllerReference(ms, vss, r.Scheme)
	})
	if err != nil {
		_ = r.setSynced(ctx, ms, metav1.ConditionFalse, ReasonSyncError, err.Error())
		return ctrl.Result{}, fmt.Errorf("vaultstaticsecret %s: %w", ms.Name, err)
	}
	return ctrl.Result{}, r.setSynced(ctx, ms, metav1.ConditionTrue, ReasonSynced,
		fmt.Sprintf("VaultStaticSecret %s syncs kv/%s/%s into Secret %s", ms.Name, ms.Namespace, ms.Name, ms.Name))
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

func (r *ManagedSecretReconciler) setSynced(ctx context.Context, ms *idpv1alpha1.ManagedSecret, status metav1.ConditionStatus, reason, msg string) error {
	before := ms.Status.DeepCopy()
	ms.Status.ObservedGeneration = ms.Generation
	meta.SetStatusCondition(&ms.Status.Conditions, metav1.Condition{
		Type:               idpv1alpha1.ConditionSynced,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: ms.Generation,
	})
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
