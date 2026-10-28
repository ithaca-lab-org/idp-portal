package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

// ensureVaultAccess creates the ServiceAccount and VaultAuth VSO uses to read
// kv/data/<ns>/*. Both are controlled by the TeamNamespace (garbage collected
// with it) and live in the namespace (deleted with it). The operator itself
// gets no Vault write access for this.
func (r *TeamNamespaceReconciler) ensureVaultAccess(ctx context.Context, tn *idpv1alpha1.TeamNamespace) error {
	sa := newVaultReaderSA(tn.Name)
	if err := r.ensureChild(ctx, tn, sa, func() { setVaultReaderSA(sa) }); err != nil {
		return fmt.Errorf("serviceaccount %s: %w", VaultReaderServiceAccount, err)
	}
	va := newVaultAuth(tn.Name)
	want := desiredVaultAuthSpec()
	if err := r.ensureChild(ctx, tn, va, func() {
		if cur, ok := va.Object["spec"].(map[string]any); !ok || !equality.Semantic.DeepEqual(cur, want) {
			va.Object["spec"] = want
		}
	}); err != nil {
		return fmt.Errorf("vaultauth %s: %w", VaultAuthName, err)
	}
	return nil
}

// initialPurge removes leftover KV data under kv/<ns>/ (for example from an
// earlier namespace of the same name whose purge gave up) before any access
// object exists. It runs once per TeamNamespace: success is recorded in
// status.vaultPurgedAt before the access objects are created, so later
// reconciles (or a tenant deleting its VaultAuth) never purge live data.
// It reports done=false when the caller must stop and requeue.
func (r *TeamNamespaceReconciler) initialPurge(ctx context.Context, tn *idpv1alpha1.TeamNamespace) (ctrl.Result, bool, error) {
	if tn.Status.VaultPurgedAt != nil {
		return ctrl.Result{}, true, nil
	}
	log := logf.FromContext(ctx)
	if err := r.Vault.purger().Purge(ctx, tn.Name); err != nil {
		log.Info("Vault purge before granting access failed, will retry", "error", err.Error())
		serr := r.setStatus(ctx, tn, idpv1alpha1.PhaseReady, metav1.ConditionTrue, ReasonReconciled,
			"namespace and guardrails are in place", tn.Name,
			vaultCondition(metav1.ConditionFalse, ReasonVaultPurgePending,
				fmt.Sprintf("purging leftover kv/%s/ before granting access failed, retrying: %v", tn.Name, err), tn))
		elapsed := r.Vault.now().Sub(tn.CreationTimestamp.Time)
		return ctrl.Result{RequeueAfter: purgeBackoff(elapsed, 24*time.Hour)}, false, serr
	}
	now := metav1.NewTime(r.Vault.now())
	tn.Status.VaultPurgedAt = &now
	if err := r.Status().Update(ctx, tn); err != nil {
		return ctrl.Result{}, false, err
	}
	log.Info("purged leftover Vault KV data before granting access")
	return ctrl.Result{}, true, nil
}

// finalizeVault purges kv/<ns>/ and then drops VaultFinalizerName. While Vault
// fails it keeps the finalizer, sets VaultPurgePending and requeues with
// backoff; once GiveUpAfter has passed since deletion it logs and lets go, so a
// destroyed Vault cannot block deletion forever.
func (r *TeamNamespaceReconciler) finalizeVault(ctx context.Context, tn *idpv1alpha1.TeamNamespace) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	switch {
	case r.Vault == nil:
		log.Info("Vault integration disabled; dropping the Vault purge finalizer without purging")
	case IsReserved(tn.Name):
		log.Info("reserved name; not purging Vault")
	default:
		// The KV prefix belongs to whoever holds the namespace now: skip the purge
		// when a namespace of that name exists and is not managed by this resource.
		ns := &corev1.Namespace{}
		if err := r.Get(ctx, types.NamespacedName{Name: tn.Name}, ns); err == nil && !managedBy(ns, tn) {
			log.Info("namespace is not managed by this TeamNamespace; not purging Vault")
			break
		} else if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		err := r.Vault.purger().Purge(ctx, tn.Name)
		if errors.Is(err, ErrInvalidNamespace) {
			log.Error(err, "not purging Vault for an invalid namespace name")
			err = nil
		}
		if err != nil {
			elapsed := r.Vault.now().Sub(tn.DeletionTimestamp.Time)
			if elapsed < r.Vault.GiveUpAfter {
				msg := fmt.Sprintf("purging kv/%s/ failed, retrying until %s: %v", tn.Name,
					tn.DeletionTimestamp.Add(r.Vault.GiveUpAfter).UTC().Format(time.RFC3339), err)
				log.Info("Vault purge failed, will retry", "error", err.Error())
				if serr := r.setPurgePending(ctx, tn, msg); serr != nil {
					return ctrl.Result{}, serr
				}
				return ctrl.Result{RequeueAfter: purgeBackoff(elapsed, r.Vault.GiveUpAfter)}, nil
			}
			log.Error(err, "giving up on the Vault purge; KV data for this namespace may remain in Vault",
				"giveUpAfter", r.Vault.GiveUpAfter.String())
		} else {
			log.Info("purged Vault KV data for namespace")
		}
	}
	controllerutil.RemoveFinalizer(tn, VaultFinalizerName)
	return ctrl.Result{}, r.Update(ctx, tn)
}

// purgeBackoff grows with the time already spent (10s up to 5m) and never
// sleeps past the give-up deadline by more than a second.
func purgeBackoff(elapsed, giveUp time.Duration) time.Duration {
	d := min(max(elapsed/4, 10*time.Second), 5*time.Minute)
	if left := giveUp - elapsed + time.Second; left > 0 && left < d {
		d = left
	}
	return d
}

func (r *TeamNamespaceReconciler) setPurgePending(ctx context.Context, tn *idpv1alpha1.TeamNamespace, msg string) error {
	before := tn.Status.DeepCopy()
	meta.SetStatusCondition(&tn.Status.Conditions, metav1.Condition{
		Type:               idpv1alpha1.ConditionVaultPurgePending,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonVaultPurgeRetry,
		Message:            msg,
		ObservedGeneration: tn.Generation,
	})
	if equality.Semantic.DeepEqual(before, &tn.Status) {
		return nil
	}
	return r.Status().Update(ctx, tn)
}
