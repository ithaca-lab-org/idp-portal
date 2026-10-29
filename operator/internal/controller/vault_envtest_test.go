package controller

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

// syncBuffer collects log output for the "never logs credentials" checks.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// reconcileVault runs one reconcile with Vault enabled, logging (at debug
// level, so everything) into logs.
func reconcileVault(t *testing.T, c client.Client, o *VaultOptions, name string, logs *syncBuffer) ctrl.Result {
	t.Helper()
	r := &TeamNamespaceReconciler{Client: c, Scheme: testScheme, Vault: o}
	lctx := logf.IntoContext(context.Background(), zap.New(zap.WriteTo(logs), zap.UseDevMode(true)))
	res, err := r.Reconcile(lctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
	return res
}

func getVaultAuth(t *testing.T, ns string) *unstructured.Unstructured {
	t.Helper()
	return get(t, newVaultAuth(""), ns, VaultAuthName)
}

func TestVaultDisabledCreatesNothing(t *testing.T) {
	requireEnvtest(t)
	name := "team-novault"
	createAndReconcile(t, newTN(name))

	if err := k8s.Get(ctx, types.NamespacedName{Namespace: name, Name: VaultReaderServiceAccount}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Errorf("ServiceAccount %s: want NotFound, got %v", VaultReaderServiceAccount, err)
	}
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: name, Name: VaultAuthName}, newVaultAuth("")); !apierrors.IsNotFound(err) {
		t.Errorf("VaultAuth: want NotFound, got %v", err)
	}
	tn := getTN(t, name)
	if controllerutil.ContainsFinalizer(tn, VaultFinalizerName) {
		t.Error("Vault finalizer added with Vault disabled")
	}
	if meta.FindStatusCondition(tn.Status.Conditions, idpv1alpha1.ConditionVaultReady) != nil {
		t.Error("VaultReady condition set with Vault disabled")
	}
}

func TestVaultAccessObjectsAreCreated(t *testing.T) {
	requireEnvtest(t)
	fv := newFakeVault(t)
	o := fv.options(t)
	logs := &syncBuffer{}
	name := "team-vault"
	if err := k8s.Create(ctx, newTN(name)); err != nil {
		t.Fatal(err)
	}
	reconcileVault(t, k8s, o, name, logs)

	tn := getTN(t, name)
	assertStatus(t, tn, idpv1alpha1.PhaseReady, ReasonReconciled)
	if c := meta.FindStatusCondition(tn.Status.Conditions, idpv1alpha1.ConditionVaultReady); c == nil ||
		c.Status != metav1.ConditionTrue || c.Reason != ReasonVaultAccessReady {
		t.Errorf("VaultReady = %+v", c)
	}
	for _, f := range []string{FinalizerName, VaultFinalizerName} {
		if !controllerutil.ContainsFinalizer(tn, f) {
			t.Errorf("finalizer %s missing", f)
		}
	}

	sa := get(t, &corev1.ServiceAccount{}, name, VaultReaderServiceAccount)
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Errorf("automountServiceAccountToken = %v, want false", sa.AutomountServiceAccountToken)
	}
	if len(sa.Secrets) != 0 || len(sa.ImagePullSecrets) != 0 {
		t.Errorf("unexpected SA secrets: %+v", sa)
	}

	va := getVaultAuth(t, name)
	wantSpec := map[string]any{
		"method":             "kubernetes",
		"mount":              "kubernetes",
		"vaultConnectionRef": "vault-secrets-operator/idp-vault",
		"kubernetes": map[string]any{
			"role":                   "ns-reader",
			"serviceAccount":         "vault-reader",
			"audiences":              []any{"vault"},
			"tokenExpirationSeconds": int64(600),
		},
	}
	if got := va.Object["spec"]; !equality.Semantic.DeepEqual(got, wantSpec) {
		t.Errorf("VaultAuth spec = %#v\nwant %#v", got, wantSpec)
	}

	for _, obj := range []client.Object{sa, va} {
		if !metav1.IsControlledBy(obj, tn) {
			t.Errorf("%s %s is not controlled by the TeamNamespace (no garbage collection)", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName())
		}
		if l := obj.GetLabels(); l[LabelManagedBy] != ManagedByValue || l[LabelTeamNamespace] != name {
			t.Errorf("%s labels = %v", obj.GetName(), l)
		}
	}

	// Second pass writes nothing.
	rvSA, rvVA, rvTN := sa.ResourceVersion, va.GetResourceVersion(), tn.ResourceVersion
	reconcileVault(t, k8s, o, name, logs)
	if get(t, &corev1.ServiceAccount{}, name, VaultReaderServiceAccount).ResourceVersion != rvSA ||
		getVaultAuth(t, name).GetResourceVersion() != rvVA || getTN(t, name).ResourceVersion != rvTN {
		t.Error("second reconcile rewrote Vault objects or the TeamNamespace")
	}

	// Drift (a tenant points the VaultAuth at another role) is reverted.
	va = getVaultAuth(t, name)
	_ = unstructured.SetNestedField(va.Object, "idp-operator-purge", "spec", "kubernetes", "role")
	if err := k8s.Update(ctx, va); err != nil {
		t.Fatal(err)
	}
	reconcileVault(t, k8s, o, name, logs)
	if role, _, _ := unstructured.NestedString(getVaultAuth(t, name).Object, "spec", "kubernetes", "role"); role != VaultReaderRole {
		t.Errorf("VaultAuth role drift not reverted: %q", role)
	}

	// Only the one-time purge before the first grant talks to Vault.
	calls, _ := fv.snapshot()
	want := []string{"POST /v1/auth/kubernetes/login", "GET /v1/kv/metadata/" + name + "?list=true", "POST /v1/auth/token/revoke-self"}
	if !slices.Equal(calls, want) {
		t.Errorf("Vault calls = %v, want only the initial purge %v", calls, want)
	}
	if getTN(t, name).Status.VaultPurgedAt == nil {
		t.Error("status.vaultPurgedAt not set")
	}
}

// Access is granted only after leftover data under kv/<ns>/ is purged, and the
// purge never runs again once access exists.
func TestVaultAccessWaitsForPurge(t *testing.T) {
	requireEnvtest(t)
	name := "team-gate"
	fv := newFakeVault(t, name+"/old/secret", "team-other/keep")
	o := fv.options(t)
	logs := &syncBuffer{}
	fv.mu.Lock()
	fv.failStatus = http.StatusServiceUnavailable
	fv.mu.Unlock()
	if err := k8s.Create(ctx, newTN(name)); err != nil {
		t.Fatal(err)
	}
	if res := reconcileVault(t, k8s, o, name, logs); res.RequeueAfter < 10*time.Second {
		t.Errorf("want a backoff requeue while Vault is down, got %+v", res)
	}
	tn := getTN(t, name)
	assertStatus(t, tn, idpv1alpha1.PhaseReady, ReasonReconciled)
	c := meta.FindStatusCondition(tn.Status.Conditions, idpv1alpha1.ConditionVaultReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonVaultPurgePending {
		t.Fatalf("VaultReady = %+v", c)
	}
	noSecrets(t, "condition message", c.Message)
	if tn.Status.VaultPurgedAt != nil {
		t.Error("vaultPurgedAt set although the purge failed")
	}
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: name, Name: VaultReaderServiceAccount}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Errorf("ServiceAccount created before the purge: %v", err)
	}
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: name, Name: VaultAuthName}, newVaultAuth("")); !apierrors.IsNotFound(err) {
		t.Errorf("VaultAuth created before the purge: %v", err)
	}

	fv.mu.Lock()
	fv.failStatus = 0
	fv.mu.Unlock()
	reconcileVault(t, k8s, o, name, logs)
	if _, remaining := fv.snapshot(); !slices.Equal(remaining, []string{"team-other/keep"}) {
		t.Errorf("leftover data not purged: %v", remaining)
	}
	get(t, &corev1.ServiceAccount{}, name, VaultReaderServiceAccount)
	getVaultAuth(t, name)
	tn = getTN(t, name)
	if c := meta.FindStatusCondition(tn.Status.Conditions, idpv1alpha1.ConditionVaultReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("VaultReady = %+v", c)
	}

	// The tenant writes a secret and deletes its VaultAuth: it is recreated and
	// the live secret is not purged.
	fv.mu.Lock()
	fv.data[name+"/app/live"] = true
	before := len(fv.calls)
	fv.mu.Unlock()
	if err := k8s.Delete(ctx, getVaultAuth(t, name)); err != nil {
		t.Fatal(err)
	}
	reconcileVault(t, k8s, o, name, logs)
	getVaultAuth(t, name)
	calls, remaining := fv.snapshot()
	if len(calls) != before || !slices.Contains(remaining, name+"/app/live") {
		t.Errorf("live data touched after access existed: calls=%v remaining=%v", calls[before:], remaining)
	}
	noSecrets(t, "logs", logs.String())
}

// A namespace that is no longer managed by the TeamNamespace keeps its KV data.
func TestVaultPurgeSkipsUnmanagedNamespace(t *testing.T) {
	requireEnvtest(t)
	name := "team-vault-unlabelled"
	fv := newFakeVault(t)
	o := fv.options(t)
	if err := k8s.Create(ctx, newTN(name)); err != nil {
		t.Fatal(err)
	}
	reconcileVault(t, k8s, o, name, &syncBuffer{})
	fv.mu.Lock()
	fv.data[name+"/app/db"] = true
	before := len(fv.calls)
	fv.mu.Unlock()

	ns := get(t, &corev1.Namespace{}, "", name)
	delete(ns.Labels, LabelManagedBy)
	if err := k8s.Update(ctx, ns); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Delete(ctx, getTN(t, name)); err != nil {
		t.Fatal(err)
	}
	reconcileVault(t, k8s, o, name, &syncBuffer{})
	if err := k8s.Get(ctx, types.NamespacedName{Name: name}, &idpv1alpha1.TeamNamespace{}); !apierrors.IsNotFound(err) {
		t.Errorf("TeamNamespace still present: %v", err)
	}
	calls, remaining := fv.snapshot()
	if len(calls) != before || len(remaining) != 1 {
		t.Errorf("unmanaged namespace was purged: calls=%v remaining=%v", calls[before:], remaining)
	}
}

func TestVaultPurgeOnDeletion(t *testing.T) {
	requireEnvtest(t)
	name := "team-purge"
	fv := newFakeVault(t, name+"/app/db", name+"/app/api", name+"/top", "team-other/app/db")
	o := fv.options(t)
	logs := &syncBuffer{}
	if err := k8s.Create(ctx, newTN(name)); err != nil {
		t.Fatal(err)
	}
	reconcileVault(t, k8s, o, name, logs)
	if err := k8s.Delete(ctx, getTN(t, name)); err != nil {
		t.Fatal(err)
	}
	if res := reconcileVault(t, k8s, o, name, logs); res.RequeueAfter != 0 {
		t.Errorf("unexpected requeue %+v", res)
	}

	if err := k8s.Get(ctx, types.NamespacedName{Name: name}, &idpv1alpha1.TeamNamespace{}); !apierrors.IsNotFound(err) {
		t.Errorf("TeamNamespace still present: %v", err)
	}
	ns := &corev1.Namespace{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: name}, ns); err == nil && ns.DeletionTimestamp.IsZero() {
		t.Error("managed namespace was not deleted")
	}
	calls, remaining := fv.snapshot()
	if !slices.Equal(remaining, []string{"team-other/app/db"}) {
		t.Errorf("remaining Vault data = %v", remaining)
	}
	wantOnlyNamespaceCalls(t, name, calls)
	noSecrets(t, "logs", logs.String())
}

func TestVaultDownKeepsFinalizerUntilGiveUp(t *testing.T) {
	requireEnvtest(t)
	name := "team-vaultdown"
	fv := newFakeVault(t, name+"/app/db")
	o := fv.options(t)
	logs := &syncBuffer{}
	if err := k8s.Create(ctx, newTN(name)); err != nil {
		t.Fatal(err)
	}
	reconcileVault(t, k8s, o, name, logs)
	if err := k8s.Delete(ctx, getTN(t, name)); err != nil {
		t.Fatal(err)
	}

	fv.mu.Lock()
	fv.failStatus = http.StatusServiceUnavailable
	fv.mu.Unlock()
	res := reconcileVault(t, k8s, o, name, logs)
	if res.RequeueAfter < 10*time.Second {
		t.Errorf("want a backoff requeue, got %+v", res)
	}
	tn := getTN(t, name)
	if !controllerutil.ContainsFinalizer(tn, VaultFinalizerName) || controllerutil.ContainsFinalizer(tn, FinalizerName) {
		t.Errorf("finalizers = %v, want only %s", tn.Finalizers, VaultFinalizerName)
	}
	c := meta.FindStatusCondition(tn.Status.Conditions, idpv1alpha1.ConditionVaultPurgePending)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonVaultPurgeRetry {
		t.Fatalf("VaultPurgePending = %+v", c)
	}
	noSecrets(t, "condition message", c.Message)
	// The namespace is deleted even though Vault is down.
	if ns := get(t, &corev1.Namespace{}, "", name); ns.DeletionTimestamp.IsZero() {
		t.Error("namespace deletion waited for Vault")
	}

	// Vault fully unreachable: still pending.
	fv.srv.Close()
	reconcileVault(t, k8s, o, name, logs)
	if !controllerutil.ContainsFinalizer(getTN(t, name), VaultFinalizerName) {
		t.Fatal("finalizer dropped before the give-up window")
	}

	// Past the window the finalizer goes, with an error in the log.
	o.Now = func() time.Time { return tn.DeletionTimestamp.Add(31 * time.Minute) }
	reconcileVault(t, k8s, o, name, logs)
	if err := k8s.Get(ctx, types.NamespacedName{Name: name}, &idpv1alpha1.TeamNamespace{}); !apierrors.IsNotFound(err) {
		t.Errorf("TeamNamespace still present after the give-up window: %v", err)
	}
	if !bytes.Contains([]byte(logs.String()), []byte("giving up on the Vault purge")) {
		t.Error("give-up was not logged")
	}
	noSecrets(t, "logs", logs.String())
}

// The CRD rejects such names, so the guard is exercised with a fake client.
func TestVaultPurgeSkipsInvalidName(t *testing.T) {
	fv := newFakeVault(t, "sys/x")
	name := "evil..name"
	now := metav1.Now()
	tn := newTN(name)
	tn.Finalizers = []string{VaultFinalizerName}
	tn.DeletionTimestamp = &now
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(tn).WithStatusSubresource(tn).Build()
	reconcileVault(t, c, fv.options(t), name, &syncBuffer{})
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &idpv1alpha1.TeamNamespace{}); !apierrors.IsNotFound(err) {
		t.Errorf("finalizer not released for an invalid name: %v", err)
	}
	if calls, _ := fv.snapshot(); len(calls) != 0 {
		t.Errorf("invalid name reached Vault: %v", calls)
	}
}
