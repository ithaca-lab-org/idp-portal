package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

const (
	portalUser       = "system:serviceaccount:idp-system:portal-api"
	portalPolicyName = "idp-portal-scope"
)

func testVaultOptions() *VaultOptions {
	return &VaultOptions{AuthMount: defaultVaultAuthMount, GiveUpAfter: time.Minute,
		Purger: purgerFunc(func(context.Context, string) error { return nil })}
}

// vaultNamespace creates a TeamNamespace and reconciles it with Vault on, so
// the namespace is managed and has VaultAuth vault-reader.
func vaultNamespace(t *testing.T, c client.Client, name string) {
	t.Helper()
	if err := k8s.Create(ctx, newTN(name)); err != nil {
		t.Fatal(err)
	}
	reconcileVault(t, c, testVaultOptions(), name, &syncBuffer{})
	getVaultAuth(t, name)
}

func newMS(ns, name string) *idpv1alpha1.ManagedSecret {
	return &idpv1alpha1.ManagedSecret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

func reconcileMS(t *testing.T, c client.Client, o *VaultOptions, ns, name string) ctrl.Result {
	t.Helper()
	r := &ManagedSecretReconciler{Client: c, Scheme: testScheme, Vault: o}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	if err != nil {
		t.Fatalf("reconcile managedsecret %s/%s: %v", ns, name, err)
	}
	return res
}

func getVSS(ns, name string) (*unstructured.Unstructured, error) {
	u := newVaultStaticSecret("", "")
	return u, k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, u)
}

func getSynced(t *testing.T, ns, name string) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(get(t, &idpv1alpha1.ManagedSecret{}, ns, name).Status.Conditions, idpv1alpha1.ConditionSynced)
}

// vssFor is a VaultStaticSecret with the operator's spec and an owner reference
// to a (made-up) ManagedSecret of the same name.
func vssFor(ns, name string) *unstructured.Unstructured {
	u := newVaultStaticSecret(ns, name)
	u.Object["spec"] = desiredVaultStaticSecretSpec(ns, name)
	u.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: idpv1alpha1.GroupVersion.String(), Kind: "ManagedSecret", Name: name,
		UID: "00000000-0000-0000-0000-000000000001", Controller: ptr.To(true),
	}})
	return u
}

func TestManagedSecretCreatesPinnedVaultStaticSecret(t *testing.T) {
	requireEnvtest(t)
	ns, name := "team-ms", "stripe"
	vaultNamespace(t, k8s, ns)
	if err := k8s.Create(ctx, newMS(ns, name)); err != nil {
		t.Fatal(err)
	}
	reconcileMS(t, k8s, testVaultOptions(), ns, name)

	vss, err := getVSS(ns, name)
	if err != nil {
		t.Fatal(err)
	}
	wantSpec := map[string]any{
		"vaultAuthRef":   "vault-reader",
		"mount":          "kv",
		"type":           "kv-v2",
		"path":           "team-ms/stripe",
		"refreshAfter":   "60s",
		"hmacSecretData": true,
		"destination":    map[string]any{"name": "stripe", "create": true, "overwrite": false},
	}
	if got := vss.Object["spec"]; !equality.Semantic.DeepEqual(got, wantSpec) {
		t.Errorf("VaultStaticSecret spec = %#v\nwant %#v", got, wantSpec)
	}
	ms := get(t, &idpv1alpha1.ManagedSecret{}, ns, name)
	if !metav1.IsControlledBy(vss, ms) || len(vss.GetOwnerReferences()) != 1 {
		t.Errorf("VaultStaticSecret owners = %+v, want only the ManagedSecret as controller", vss.GetOwnerReferences())
	}
	if l := vss.GetLabels(); l[LabelManagedBy] != ManagedByValue || l[LabelManagedSecret] != name {
		t.Errorf("labels = %v", l)
	}
	if c := getSynced(t, ns, name); c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonSynced {
		t.Errorf("Synced = %+v", c)
	}
	if ms.Status.ObservedGeneration != ms.Generation {
		t.Errorf("observedGeneration = %d, want %d", ms.Status.ObservedGeneration, ms.Generation)
	}
	if c := getCond(t, ns, name, idpv1alpha1.ConditionDelivered); c == nil || c.Status != metav1.ConditionUnknown || c.Reason != ReasonDeliveryPending {
		t.Errorf("Delivered before VSO reported = %+v", c)
	}

	// A second pass changes nothing.
	rv := vss.GetResourceVersion()
	msRV := ms.ResourceVersion
	reconcileMS(t, k8s, testVaultOptions(), ns, name)
	vss, _ = getVSS(ns, name)
	if vss.GetResourceVersion() != rv || get(t, &idpv1alpha1.ManagedSecret{}, ns, name).ResourceVersion != msRV {
		t.Error("second reconcile wrote objects")
	}

	// Drift is reverted, including fields the operator never sets.
	_ = unstructured.SetNestedField(vss.Object, "team-other/stripe", "spec", "path")
	_ = unstructured.SetNestedSlice(vss.Object, []any{map[string]any{"kind": "Deployment", "name": "x"}}, "spec", "rolloutRestartTargets")
	if err := k8s.Update(ctx, vss); err != nil {
		t.Fatal(err)
	}
	reconcileMS(t, k8s, testVaultOptions(), ns, name)
	vss, _ = getVSS(ns, name)
	if got := vss.Object["spec"]; !equality.Semantic.DeepEqual(got, wantSpec) {
		t.Errorf("drift not reverted: %#v", got)
	}

	// Delivered follows VSO's SecretSynced condition for the current generation.
	for _, tc := range []struct {
		status, reason string
		gen            int64
		want           metav1.ConditionStatus
	}{
		{"True", ReasonDelivered, vss.GetGeneration(), metav1.ConditionTrue},
		{"False", ReasonDeliveryFailed, vss.GetGeneration(), metav1.ConditionFalse},
		{"True", ReasonDeliveryPending, vss.GetGeneration() - 1, metav1.ConditionUnknown},
	} {
		setVSOStatus(t, ns, name, tc.status, tc.gen)
		reconcileMS(t, k8s, testVaultOptions(), ns, name)
		if c := getCond(t, ns, name, idpv1alpha1.ConditionDelivered); c == nil || c.Status != tc.want || c.Reason != tc.reason {
			t.Errorf("VSO SecretSynced=%s gen %d: Delivered = %+v", tc.status, tc.gen, c)
		}
	}
}

// setVSOStatus writes the status VSO 1.6.2 would write.
func setVSOStatus(t *testing.T, ns, name, status string, gen int64) {
	t.Helper()
	vss, err := getVSS(ns, name)
	if err != nil {
		t.Fatal(err)
	}
	vss.Object["status"] = map[string]any{"conditions": []any{
		map[string]any{"type": "SecretSynced", "status": status, "reason": "Synced", "message": "Secret synced, horizon=1m",
			"observedGeneration": gen, "lastTransitionTime": "2026-01-01T00:00:00Z"},
		map[string]any{"type": "Healthy", "status": status, "reason": "Healthy", "message": "x",
			"observedGeneration": gen, "lastTransitionTime": "2026-01-01T00:00:00Z"},
	}}
	if err := k8s.Status().Update(ctx, vss); err != nil {
		t.Fatal(err)
	}
}

func getCond(t *testing.T, ns, name, typ string) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(get(t, &idpv1alpha1.ManagedSecret{}, ns, name).Status.Conditions, typ)
}

// A VaultStaticSecret of the same name that the ManagedSecret does not own
// (made by hand here) is never adopted or changed.
func TestManagedSecretNameConflict(t *testing.T) {
	requireEnvtest(t)
	ns, name := "team-ms-conflict", "stripe"
	vaultNamespace(t, k8s, ns)
	hand := vssFor(ns, name)
	hand.SetOwnerReferences(nil)
	_ = unstructured.SetNestedField(hand.Object, "platform/stripe", "spec", "path")
	if err := k8s.Create(ctx, hand); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(ctx, newMS(ns, name)); err != nil {
		t.Fatal(err)
	}
	res := reconcileMS(t, k8s, testVaultOptions(), ns, name)
	if res.RequeueAfter < time.Minute {
		t.Errorf("RequeueAfter = %v, want a slow requeue", res.RequeueAfter)
	}
	if c := getSynced(t, ns, name); c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonNameConflict {
		t.Errorf("Synced = %+v", c)
	}
	if c := getCond(t, ns, name, idpv1alpha1.ConditionDelivered); c == nil || c.Status != metav1.ConditionUnknown {
		t.Errorf("Delivered = %+v", c)
	}
	got, _ := getVSS(ns, name)
	if got.GetResourceVersion() != hand.GetResourceVersion() || len(got.GetOwnerReferences()) != 0 {
		t.Errorf("hand-made VaultStaticSecret was modified: owners %+v", got.GetOwnerReferences())
	}
}

// After a delete and re-create of a ManagedSecret, the old VaultStaticSecret
// (still controlled by the old UID, or terminating behind VSO's finalizer) is
// waited for with a requeue, not an error.
func TestManagedSecretWaitsForPreviousVaultStaticSecret(t *testing.T) {
	requireEnvtest(t)
	ns := "team-ms-prev"
	vaultNamespace(t, k8s, ns)

	old := vssFor(ns, "stripe")
	old.SetLabels(map[string]string{LabelManagedBy: ManagedByValue, LabelManagedSecret: "stripe"})
	if err := k8s.Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(ctx, newMS(ns, "stripe")); err != nil {
		t.Fatal(err)
	}
	r := &ManagedSecretReconciler{Client: k8s, Scheme: testScheme, Vault: testVaultOptions()}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "stripe"}})
	if err != nil || res.RequeueAfter == 0 || res.RequeueAfter > time.Minute {
		t.Errorf("old owner: res=%+v err=%v, want a short requeue and no error", res, err)
	}
	if c := getSynced(t, ns, "stripe"); c == nil || c.Reason != ReasonPreviousTerminating {
		t.Errorf("Synced = %+v", c)
	}
	if got, _ := getVSS(ns, "stripe"); got.GetResourceVersion() != old.GetResourceVersion() {
		t.Error("old VaultStaticSecret was modified")
	}

	// Terminating (VSO's finalizer still on it), even if it is ours.
	if err := k8s.Create(ctx, newMS(ns, "card")); err != nil {
		t.Fatal(err)
	}
	reconcileMS(t, k8s, testVaultOptions(), ns, "card")
	vss, _ := getVSS(ns, "card")
	vss.SetFinalizers([]string{"vaultstaticsecret.secrets.hashicorp.com/finalizer"})
	if err := k8s.Update(ctx, vss); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Delete(ctx, vss); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if v, err := getVSS(ns, "card"); err == nil {
			v.SetFinalizers(nil)
			_ = k8s.Update(ctx, v)
		}
	})
	res, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "card"}})
	if err != nil || res.RequeueAfter == 0 {
		t.Errorf("terminating: res=%+v err=%v, want a requeue and no error", res, err)
	}
	if c := getSynced(t, ns, "card"); c == nil || c.Reason != ReasonPreviousTerminating {
		t.Errorf("Synced = %+v", c)
	}
}

func TestManagedSecretIgnoredOutsideManagedNamespaces(t *testing.T) {
	requireEnvtest(t)
	o := testVaultOptions()
	plain := "plain-ms"
	ensurePlainNamespace(t, plain)
	// Carries the operator labels but has no TeamNamespace behind it.
	fake := "fake-managed-ms"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fake, Labels: map[string]string{
		LabelManagedBy: ManagedByValue, LabelTeamNamespace: fake,
	}}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	for _, ns := range []string{plain, fake, "kube-system"} {
		if err := k8s.Create(ctx, newMS(ns, "stripe")); err != nil {
			t.Fatal(err)
		}
		reconcileMS(t, k8s, o, ns, "stripe")
		if _, err := getVSS(ns, "stripe"); !apierrors.IsNotFound(err) {
			t.Errorf("%s: VaultStaticSecret created outside a managed namespace: %v", ns, err)
		}
		if c := getSynced(t, ns, "stripe"); c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonNamespaceNotManaged {
			t.Errorf("%s: Synced = %+v", ns, c)
		}
	}
}

func TestManagedSecretWaitsForVaultAuth(t *testing.T) {
	requireEnvtest(t)
	// Reconciled with Vault off: managed, but no VaultAuth.
	ns := "team-ms-noauth"
	createAndReconcile(t, newTN(ns))
	if err := k8s.Create(ctx, newMS(ns, "stripe")); err != nil {
		t.Fatal(err)
	}
	if res := reconcileMS(t, k8s, testVaultOptions(), ns, "stripe"); res.RequeueAfter == 0 {
		t.Error("want a requeue while the VaultAuth is missing")
	}
	if _, err := getVSS(ns, "stripe"); !apierrors.IsNotFound(err) {
		t.Errorf("VaultStaticSecret created without VaultAuth: %v", err)
	}
	if c := getSynced(t, ns, "stripe"); c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonVaultAuthMissing {
		t.Errorf("Synced = %+v", c)
	}
}

func TestManagedSecretVaultDisabledDoesNothing(t *testing.T) {
	requireEnvtest(t)
	ns := "team-ms-off"
	vaultNamespace(t, k8s, ns)
	if err := k8s.Create(ctx, newMS(ns, "stripe")); err != nil {
		t.Fatal(err)
	}
	reconcileMS(t, k8s, nil, ns, "stripe")
	if _, err := getVSS(ns, "stripe"); !apierrors.IsNotFound(err) {
		t.Errorf("VaultStaticSecret created with Vault disabled: %v", err)
	}
	if ms := get(t, &idpv1alpha1.ManagedSecret{}, ns, "stripe"); len(ms.Status.Conditions) != 0 {
		t.Errorf("status written with Vault disabled: %+v", ms.Status)
	}
}

// envtest runs no garbage collector; deletion relies on the controller owner
// reference checked in TestManagedSecretCreatesPinnedVaultStaticSecret. Here:
// the ManagedSecret carries no finalizer, so it goes at once, and reconciling
// a gone (or deleting) ManagedSecret writes nothing.
func TestManagedSecretDeletion(t *testing.T) {
	requireEnvtest(t)
	ns := "team-ms-del"
	vaultNamespace(t, k8s, ns)
	if err := k8s.Create(ctx, newMS(ns, "stripe")); err != nil {
		t.Fatal(err)
	}
	reconcileMS(t, k8s, testVaultOptions(), ns, "stripe")
	if len(get(t, &idpv1alpha1.ManagedSecret{}, ns, "stripe").Finalizers) != 0 {
		t.Error("ManagedSecret has finalizers")
	}
	if err := k8s.Delete(ctx, newMS(ns, "stripe")); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "stripe"}, &idpv1alpha1.ManagedSecret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("ManagedSecret still present: %v", err)
	}
	vss, _ := getVSS(ns, "stripe")
	rv := vss.GetResourceVersion()
	reconcileMS(t, k8s, testVaultOptions(), ns, "stripe")
	if vss, _ := getVSS(ns, "stripe"); vss.GetResourceVersion() != rv {
		t.Error("reconcile of a deleted ManagedSecret touched the VaultStaticSecret")
	}
}

func TestManagedSecretCRDValidation(t *testing.T) {
	requireEnvtest(t)
	ns := "plain-ms-crd"
	ensurePlainNamespace(t, ns)
	dry := client.DryRunAll
	for _, n := range []string{"vault-reader", "default", "default-token-abcde", "vault-reader-token-x", "sh-helm-x",
		"kube-x", "istio-x", "idp-quota", strings.Repeat("a", 41), "Upper", "a.b", "-a"} {
		if err := k8s.Create(ctx, newMS(ns, n), dry); err == nil {
			t.Errorf("name %q accepted", n)
		}
	}
	if err := k8s.Create(ctx, newMS(ns, strings.Repeat("a", 40)), dry); err != nil {
		t.Errorf("40-char name rejected: %v", err)
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": idpv1alpha1.GroupVersion.String(), "kind": "ManagedSecret",
		"metadata": map[string]any{"name": "extra", "namespace": ns},
		"spec":     map[string]any{"path": "kube-system/x", "vaultAuthRef": "other"},
	}}
	if err := k8s.Create(ctx, u.DeepCopy(), dry, client.FieldValidation("Strict")); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("strict create with unknown spec fields: %v", err)
	}
	pruned := u.DeepCopy()
	if err := k8s.Create(ctx, pruned, dry); err != nil {
		t.Fatal(err)
	}
	if spec, _, _ := unstructured.NestedMap(pruned.Object, "spec"); len(spec) != 0 {
		t.Errorf("unknown spec fields were not pruned: %v", spec)
	}
}

// TestOperatorManagedSecretUnderRBACAndPolicy runs the reconciler as the
// operator's ServiceAccount and checks that the policy pins VaultStaticSecrets.
func TestOperatorManagedSecretUnderRBACAndPolicy(t *testing.T) {
	op := operatorClient(t)
	managed, name := "team-ms-rbac", "stripe"
	vaultNamespace(t, op, managed)
	if err := k8s.Create(ctx, newMS(managed, name)); err != nil {
		t.Fatal(err)
	}
	reconcileMS(t, op, testVaultOptions(), managed, name)
	if c := getSynced(t, managed, name); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Synced = %+v", c)
	}
	plain := "plain-ns"
	ensurePlainNamespace(t, plain)
	dry := client.DryRunAll

	wantPolicyDenial(t, "VSS in kube-system", op.Create(ctx, vssFor("kube-system", name), dry))
	wantPolicyDenial(t, "VSS in unlabelled ns", op.Create(ctx, vssFor(plain, name), dry))
	wrongName := vssFor(managed, "other")
	wrongName.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: idpv1alpha1.GroupVersion.String(), Kind: "ManagedSecret",
		Name: name, UID: "00000000-0000-0000-0000-000000000001", Controller: ptr.To(true)}})
	wantPolicyDenial(t, "VSS named unlike its ManagedSecret", op.Create(ctx, wrongName, dry))
	noOwner := vssFor(managed, "orphan")
	noOwner.SetOwnerReferences(nil)
	wantPolicyDenial(t, "VSS without ManagedSecret owner", op.Create(ctx, noOwner, dry))
	twoOwners := vssFor(managed, "two")
	twoOwners.SetOwnerReferences(append(twoOwners.GetOwnerReferences(),
		metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "x", UID: "00000000-0000-0000-0000-000000000002"}))
	wantPolicyDenial(t, "VSS with a second owner", op.Create(ctx, twoOwners, dry))

	for what, mutate := range map[string]func(*unstructured.Unstructured){
		"path in another namespace": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "team-other/stripe", "spec", "path")
		},
		"path under another name": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, managed+"/other", "spec", "path")
		},
		"other mount": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "secret", "spec", "mount")
		},
		"kv-v1": func(u *unstructured.Unstructured) { _ = unstructured.SetNestedField(u.Object, "kv-v1", "spec", "type") },
		"other VaultAuth": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "default", "spec", "vaultAuthRef")
		},
		"cross-namespace VaultAuth": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "vault/vault-reader", "spec", "vaultAuthRef")
		},
		"refreshAfter": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "1s", "spec", "refreshAfter")
		},
		"no hmac": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, false, "spec", "hmacSecretData")
		},
		"overwrite true": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, true, "spec", "destination", "overwrite")
		},
		"create false": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, false, "spec", "destination", "create")
		},
		"other destination": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "vault-reader", "spec", "destination", "name")
		},
		"destination type": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "kubernetes.io/dockerconfigjson", "spec", "destination", "type")
		},
		"destination labels": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedStringMap(u.Object, map[string]string{"a": "b"}, "spec", "destination", "labels")
		},
		"destination annotations": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedStringMap(u.Object, map[string]string{"a": "b"}, "spec", "destination", "annotations")
		},
		"transformation": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, map[string]any{"excludeRaw": true}, "spec", "destination", "transformation")
		},
		"rolloutRestartTargets": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"kind": "Deployment", "name": "x"}}, "spec", "rolloutRestartTargets")
		},
		"vault namespace": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "admin", "spec", "namespace")
		},
		"version pin": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, int64(1), "spec", "version")
		},
		"syncConfig": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, map[string]any{"instantUpdates": true}, "spec", "syncConfig")
		},
	} {
		vss, err := getVSS(managed, name)
		if err != nil {
			t.Fatal(err)
		}
		mutate(vss)
		wantPolicyDenial(t, "update VSS: "+what, op.Update(ctx, vss, dry))
	}
	// A hand-made VaultStaticSecret cannot be adopted, even with a valid result.
	hand := vssFor(managed, "handmade")
	hand.SetOwnerReferences(nil)
	if err := k8s.Create(ctx, hand); err != nil {
		t.Fatal(err)
	}
	// Adding the ownerReference is refused by OwnerReferencesPermissionEnforcement
	// (no delete right) in envtest, and by the policy's oldObject check on
	// clusters without that plugin; any other write is refused by the policy.
	adopt, _ := getVSS(managed, "handmade")
	adopt.SetOwnerReferences(vssFor(managed, "handmade").GetOwnerReferences())
	wantForbidden(t, "adopt hand-made VSS", op.Update(ctx, adopt, dry))
	touch, _ := getVSS(managed, "handmade")
	touch.SetLabels(map[string]string{"x": "y"})
	wantPolicyDenial(t, "update hand-made VSS", op.Update(ctx, touch, dry))

	vss, _ := getVSS(managed, name)
	wantForbidden(t, "delete VSS", op.Delete(ctx, vss, dry))
	wantForbidden(t, "deletecollection VSS", op.DeleteAllOf(ctx, newVaultStaticSecret("", ""), client.InNamespace(managed), dry))
}

var (
	portalOnce   sync.Once
	portalClient client.Client
	portalErr    error
)

// portalAPIClient impersonates idp-system/portal-api with the RBAC idp-gitops
// gives it (workloads/portal/portal-rbac.yaml, ClusterRole idp-portal-orders).
func portalAPIClient(t *testing.T) client.Client {
	t.Helper()
	operatorClient(t) // installs config/rbac and config/policy
	portalOnce.Do(func() {
		for _, o := range []client.Object{
			&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "idp-portal-orders"}, Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{idpv1alpha1.GroupVersion.Group}, Resources: []string{"teamnamespaces"}, Verbs: []string{"get", "create"}},
				{APIGroups: []string{idpv1alpha1.GroupVersion.Group}, Resources: []string{"managedsecrets"}, Verbs: []string{"get", "create", "delete"}},
			}},
			&rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "idp-portal-orders"},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "idp-portal-orders"},
				Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "portal-api", Namespace: "idp-system"}},
			},
		} {
			if portalErr = k8s.Create(ctx, o); portalErr != nil {
				return
			}
		}
		c := rest.CopyConfig(cfg)
		c.Impersonate = rest.ImpersonationConfig{
			UserName: portalUser,
			Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:idp-system", "system:authenticated"},
		}
		portalClient, portalErr = client.New(c, client.Options{Scheme: testScheme})
	})
	if portalErr != nil {
		t.Fatalf("portal identity setup: %v", portalErr)
	}
	waitPortalPolicy(t, portalClient, true)
	return portalClient
}

func isPortalDenial(err error) bool {
	return apierrors.IsForbidden(err) && strings.Contains(err.Error(), portalPolicyName)
}

func waitPortalPolicy(t *testing.T, pc client.Client, active bool) {
	t.Helper()
	ensurePlainNamespace(t, "plain-ns")
	eventually(t, fmt.Sprintf("portal policy active=%v", active), func() error {
		err := pc.Create(ctx, newMS("plain-ns", "probe"), client.DryRunAll)
		if isPortalDenial(err) != active {
			return fmt.Errorf("probe create: %v", err)
		}
		return nil
	})
	if active {
		p := &admissionv1.ValidatingAdmissionPolicy{}
		if err := k8s.Get(ctx, types.NamespacedName{Name: portalPolicyName}, p); err != nil {
			t.Fatal(err)
		}
		if tc := p.Status.TypeChecking; tc != nil && len(tc.ExpressionWarnings) > 0 {
			t.Fatalf("portal policy type-check warnings: %+v", tc.ExpressionWarnings)
		}
	}
}

func TestPortalManagedSecretScope(t *testing.T) {
	pc := portalAPIClient(t)
	managed := "team-ms-portal"
	createAndReconcile(t, newTN(managed))
	plain := "plain-ns"
	dry := client.DryRunAll

	// Allowed: create, get and delete one ManagedSecret in a managed namespace.
	if err := pc.Create(ctx, newMS(managed, "stripe")); err != nil {
		t.Fatalf("portal create in managed ns: %v", err)
	}
	ms := &idpv1alpha1.ManagedSecret{}
	if err := pc.Get(ctx, types.NamespacedName{Namespace: managed, Name: "stripe"}, ms); err != nil {
		t.Fatalf("portal get: %v", err)
	}

	wantPortal := func(what string, err error) {
		t.Helper()
		if !isPortalDenial(err) {
			t.Errorf("%s: want denial by %s, got %v", what, portalPolicyName, err)
		}
	}
	wantPortal("create in unlabelled ns", pc.Create(ctx, newMS(plain, "stripe"), dry))
	wantPortal("create in kube-system", pc.Create(ctx, newMS("kube-system", "stripe"), dry))
	wantPortal("create in idp-system", pc.Create(ctx, newMS("idp-system", "stripe"), dry))
	withFinalizer := newMS(managed, "fin")
	withFinalizer.Finalizers = []string{"example.com/block"}
	wantPortal("create with a finalizer", pc.Create(ctx, withFinalizer, dry))
	withOwner := newMS(managed, "owned")
	withOwner.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "x", UID: "00000000-0000-0000-0000-000000000003"}}
	wantPortal("create with an ownerReference", pc.Create(ctx, withOwner, dry))
	if err := k8s.Create(ctx, newMS(plain, "admin-made")); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	wantPortal("delete in unlabelled ns", pc.Delete(ctx, newMS(plain, "admin-made"), dry))

	extra := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": idpv1alpha1.GroupVersion.String(), "kind": "ManagedSecret",
		"metadata": map[string]any{"name": "extra", "namespace": managed},
		"spec":     map[string]any{"path": "kube-system/x", "rolloutRestartTargets": []any{"x"}},
	}}
	if err := pc.Create(ctx, extra.DeepCopy(), dry, client.FieldValidation("Strict")); err == nil {
		t.Error("strict create with extra spec fields accepted")
	}
	if err := pc.Create(ctx, extra, dry); err != nil {
		t.Fatal(err)
	} else if spec, _, _ := unstructured.NestedMap(extra.Object, "spec"); len(spec) != 0 {
		t.Errorf("extra spec fields stored: %v", spec)
	}

	// RBAC: no list, watch, update, patch, status or other kinds.
	wantForbidden(t, "list managedsecrets", pc.List(ctx, &idpv1alpha1.ManagedSecretList{}, client.InNamespace(managed)))
	wantForbidden(t, "list managedsecrets cluster-wide", pc.List(ctx, &idpv1alpha1.ManagedSecretList{}))
	upd := ms.DeepCopy()
	upd.Labels = map[string]string{"x": "y"}
	wantForbidden(t, "update managedsecret", pc.Update(ctx, upd, dry))
	wantForbidden(t, "patch managedsecret", pc.Patch(ctx, upd, client.MergeFrom(ms), dry))
	st := ms.DeepCopy()
	st.Status.ObservedGeneration = 99
	wantForbidden(t, "update managedsecret status", pc.Status().Update(ctx, st, dry))
	wantForbidden(t, "deletecollection managedsecrets", pc.DeleteAllOf(ctx, &idpv1alpha1.ManagedSecret{}, client.InNamespace(managed), dry))
	wantForbidden(t, "create VaultStaticSecret", pc.Create(ctx, vssFor(managed, "stripe"), dry))
	wantForbidden(t, "create Secret", pc.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: managed}}, dry))
	wantForbidden(t, "create ConfigMap", pc.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: managed}}, dry))
	wantForbidden(t, "delete TeamNamespace", pc.Delete(ctx, newTN(managed), dry))

	if err := pc.Delete(ctx, newMS(managed, "stripe")); err != nil {
		t.Errorf("portal delete in managed ns: %v", err)
	}
}

// TestPortalPolicyIsWhatDenies removes the portal binding and shows that RBAC
// alone would let the portal write ManagedSecrets anywhere.
func TestPortalPolicyIsWhatDenies(t *testing.T) {
	pc := portalAPIClient(t)
	binding := &admissionv1.ValidatingAdmissionPolicyBinding{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: portalPolicyName}, binding); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Delete(ctx, binding); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		restored := &admissionv1.ValidatingAdmissionPolicyBinding{
			ObjectMeta: metav1.ObjectMeta{Name: portalPolicyName},
			Spec:       *binding.Spec.DeepCopy(),
		}
		if err := k8s.Create(ctx, restored); err != nil {
			t.Fatal(err)
		}
		waitPortalPolicy(t, pc, true)
	})
	waitPortalPolicy(t, pc, false)

	dry := client.DryRunAll
	if err := pc.Create(ctx, newMS("kube-system", "rbac-only"), dry); err != nil {
		t.Errorf("without the policy, RBAC should allow a ManagedSecret in kube-system (dry-run): %v", err)
	}
	fin := newMS("plain-ns", "fin")
	fin.Finalizers = []string{"example.com/block"}
	if err := pc.Create(ctx, fin, dry); err != nil {
		t.Errorf("without the policy, RBAC should allow a finalizer (dry-run): %v", err)
	}
}
