package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

const testBase = "apps.ithaca-idp.com"

func testCert(ns, name string, dns ...string) *unstructured.Unstructured {
	u := newUnstructured(CertificateGVK, ns, name)
	_ = unstructured.SetNestedStringSlice(u.Object, dns, "spec", "dnsNames")
	_ = unstructured.SetNestedField(u.Object, "wildcard-apps-tls", "spec", "secretName")
	_ = unstructured.SetNestedField(u.Object, "letsencrypt", "spec", "issuerRef", "name")
	_ = unstructured.SetNestedField(u.Object, "ClusterIssuer", "spec", "issuerRef", "kind")
	_ = unstructured.SetNestedField(u.Object, "2160h", "spec", "duration")
	return u
}

func createTestCert(t *testing.T, ns, name string, dns ...string) {
	t.Helper()
	ensurePlainNamespace(t, ns)
	if err := k8s.Create(ctx, testCert(ns, name, dns...)); err != nil {
		t.Fatalf("create certificate %s/%s: %v", ns, name, err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, newUnstructured(CertificateGVK, ns, name)) })
}

func getCert(t *testing.T, ns, name string) *unstructured.Unstructured {
	t.Helper()
	u := newUnstructured(CertificateGVK, ns, name)
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, u); err != nil {
		t.Fatalf("get certificate %s/%s: %v", ns, name, err)
	}
	return u
}

func certDNS(t *testing.T, ns, name string) []string {
	t.Helper()
	got, _, err := unstructured.NestedStringSlice(getCert(t, ns, name).Object, "spec", "dnsNames")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func tcOptions(ns, name string) TenantCertOptions {
	return TenantCertOptions{Namespace: ns, Name: name, BaseDomain: testBase}
}

func reconcileTenantCert(t *testing.T, c client.Client, o TenantCertOptions) ctrl.Result {
	t.Helper()
	res, err := (&TenantCertReconciler{Client: c, Options: o}).Reconcile(ctx, tenantCertKey)
	if err != nil {
		t.Fatalf("reconcile tenant certificate: %v", err)
	}
	return res
}

// createBareTN creates a TeamNamespace without running its reconciler (no
// namespace, no finalizer), which is all the tenant certificate looks at.
func createBareTN(t *testing.T, name string, finalizers ...string) {
	t.Helper()
	tn := newTN(name)
	tn.Finalizers = finalizers
	if err := k8s.Create(ctx, tn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		got := &idpv1alpha1.TeamNamespace{}
		if err := k8s.Get(ctx, types.NamespacedName{Name: name}, got); err != nil {
			return
		}
		got.Finalizers = nil
		_ = k8s.Update(ctx, got)
		_ = k8s.Delete(ctx, got)
	})
}

// liveTNs lists the TeamNamespaces that are not being deleted.
func liveTNs(t *testing.T) []TenantNamespace {
	t.Helper()
	list := &idpv1alpha1.TeamNamespaceList{}
	if err := k8s.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	var out []TenantNamespace
	for _, tn := range list.Items {
		if tn.DeletionTimestamp == nil {
			out = append(out, TenantNamespace{Name: tn.Name, Created: tn.CreationTimestamp.Time})
		}
	}
	return out
}

func tenantName(ns string) string { return "*." + ns + "." + testBase }

func TestDesiredDNSNamesCapAndFilter(t *testing.T) {
	o := tcOptions("istio-ingress", "wildcard-apps")
	t0 := time.Now()
	var in []TenantNamespace
	for i := 0; i < 150; i++ {
		in = append(in, TenantNamespace{Name: fmt.Sprintf("team-%03d", i), Created: t0.Add(time.Duration(i) * time.Second)})
	}
	for _, n := range []string{"vault", "kube-x", "default", strings.Repeat("a", 41), "Bad_Name"} {
		in = append(in, TenantNamespace{Name: n, Created: t0.Add(-time.Hour)})
	}
	in = append(in, TenantNamespace{Name: "team-001", Created: t0.Add(time.Second)})
	slices.Reverse(in)
	got, skipped, invalid := o.DesiredDNSNames(in)
	if len(got) != 100 || got[0] != "*."+testBase || got[1] != tenantName("team-000") || got[99] != tenantName("team-098") {
		t.Errorf("got %d names, first %q second %q last %q", len(got), got[0], got[1], got[len(got)-1])
	}
	if !slices.IsSorted(got[1:]) {
		t.Error("tenant names are not sorted")
	}
	if len(skipped) != 51 || skipped[0] != "team-099" {
		t.Errorf("skipped %d, first %v", len(skipped), skipped[:1])
	}
	if len(invalid) != 5 {
		t.Errorf("invalid = %v", invalid)
	}
	if again, _, _ := o.DesiredDNSNames(in); !slices.Equal(again, got) {
		t.Error("not deterministic")
	}
	if got, _, _ := o.DesiredDNSNames(nil); !slices.Equal(got, []string{"*." + testBase}) {
		t.Errorf("no namespaces: %v", got)
	}
	for suffix, want := range map[string]string{"apps.ithaca-idp.com": testBase, ".apps.ithaca-idp.com": testBase} {
		if b, err := TenantCertBaseDomain(suffix); err != nil || b != want {
			t.Errorf("base of %q = %q, %v", suffix, b, err)
		}
	}
	if _, err := TenantCertBaseDomain(".."); err == nil {
		t.Error("invalid suffix accepted")
	}
}

// At the cap a newer namespace never evicts an older one, whatever its name;
// equal creation times fall back to the name.
func TestDesiredDNSNamesKeepsOldestAtCap(t *testing.T) {
	o := tcOptions("istio-ingress", "wildcard-apps")
	o.MaxNames = 2
	t0 := time.Now()
	in := []TenantNamespace{
		{Name: "zzz", Created: t0}, {Name: "mmm", Created: t0.Add(time.Second)},
		{Name: "aaa", Created: t0.Add(2 * time.Second)},
	}
	got, skipped, _ := o.DesiredDNSNames(in)
	if want := []string{"*." + testBase, tenantName("mmm"), tenantName("zzz")}; !slices.Equal(got, want) || !slices.Equal(skipped, []string{"aaa"}) {
		t.Errorf("got %v skipped %v", got, skipped)
	}
	ties := []TenantNamespace{{Name: "b", Created: t0}, {Name: "c", Created: t0}, {Name: "a", Created: t0}}
	if got, skipped, _ := o.DesiredDNSNames(ties); !slices.Equal(got, []string{"*." + testBase, tenantName("a"), tenantName("b")}) || !slices.Equal(skipped, []string{"c"}) {
		t.Errorf("ties: got %v skipped %v", got, skipped)
	}
}

func TestTenantCertAddsAndRemovesNames(t *testing.T) {
	requireEnvtest(t)
	o := tcOptions("tc-add", "wildcard-apps")
	createTestCert(t, o.Namespace, o.Name, "*."+testBase)
	createBareTN(t, "tc-add-b")
	createBareTN(t, "tc-add-a")

	reconcileTenantCert(t, k8s, o)
	got := certDNS(t, o.Namespace, o.Name)
	want, _, _ := o.DesiredDNSNames(liveTNs(t))
	if !slices.Equal(got, want) || got[0] != "*."+testBase {
		t.Fatalf("dnsNames = %v, want %v", got, want)
	}
	for _, n := range []string{"tc-add-a", "tc-add-b"} {
		if !slices.Contains(got, tenantName(n)) {
			t.Errorf("%s missing from %v", n, got)
		}
	}
	// Nothing but dnsNames was touched.
	c := getCert(t, o.Namespace, o.Name)
	if v, _, _ := unstructured.NestedString(c.Object, "spec", "secretName"); v != "wildcard-apps-tls" {
		t.Errorf("secretName = %q", v)
	}
	if v, _, _ := unstructured.NestedString(c.Object, "spec", "duration"); v != "2160h0m0s" && v != "2160h" {
		t.Errorf("duration = %q", v)
	}

	if err := k8s.Delete(ctx, newTN("tc-add-a")); err != nil {
		t.Fatal(err)
	}
	reconcileTenantCert(t, k8s, o)
	got = certDNS(t, o.Namespace, o.Name)
	if slices.Contains(got, tenantName("tc-add-a")) || !slices.Contains(got, tenantName("tc-add-b")) {
		t.Errorf("after delete: %v", got)
	}
}

func TestTenantCertIgnoresNamespacesBeingDeleted(t *testing.T) {
	requireEnvtest(t)
	o := tcOptions("tc-deleting", "wildcard-apps")
	createTestCert(t, o.Namespace, o.Name, "*."+testBase)
	createBareTN(t, "tc-deleting-x", "test.ithaca-idp.com/hold")
	reconcileTenantCert(t, k8s, o)
	if !slices.Contains(certDNS(t, o.Namespace, o.Name), tenantName("tc-deleting-x")) {
		t.Fatal("live namespace not listed")
	}
	if err := k8s.Delete(ctx, newTN("tc-deleting-x")); err != nil {
		t.Fatal(err)
	}
	if got := get(t, &idpv1alpha1.TeamNamespace{}, "", "tc-deleting-x"); got.DeletionTimestamp == nil {
		t.Fatal("TeamNamespace should be held by its finalizer")
	}
	reconcileTenantCert(t, k8s, o)
	if slices.Contains(certDNS(t, o.Namespace, o.Name), tenantName("tc-deleting-x")) {
		t.Error("namespace being deleted is still listed")
	}
}

func TestTenantCertNoOpWhenEqual(t *testing.T) {
	requireEnvtest(t)
	o := tcOptions("tc-noop", "wildcard-apps")
	createTestCert(t, o.Namespace, o.Name, "*."+testBase)
	createBareTN(t, "tc-noop-a")
	reconcileTenantCert(t, k8s, o)
	rv := getCert(t, o.Namespace, o.Name).GetResourceVersion()
	for i := 0; i < 3; i++ {
		reconcileTenantCert(t, k8s, o)
	}
	if got := getCert(t, o.Namespace, o.Name).GetResourceVersion(); got != rv {
		t.Errorf("resourceVersion changed %s -> %s on a no-op", rv, got)
	}
}

func TestTenantCertRepairsDrift(t *testing.T) {
	requireEnvtest(t)
	o := tcOptions("tc-drift", "wildcard-apps")
	createTestCert(t, o.Namespace, o.Name, "*."+testBase)
	createBareTN(t, "tc-drift-a")
	reconcileTenantCert(t, k8s, o)
	want := certDNS(t, o.Namespace, o.Name)

	for name, drifted := range map[string][]string{
		"stripped": {"*." + testBase},
		"junk":     {"*." + testBase, "*.evil.com"},
		"reorder":  {tenantName("tc-drift-a"), "*." + testBase},
	} {
		c := getCert(t, o.Namespace, o.Name)
		if err := unstructured.SetNestedStringSlice(c.Object, drifted, "spec", "dnsNames"); err != nil {
			t.Fatal(err)
		}
		if err := k8s.Update(ctx, c); err != nil {
			t.Fatal(err)
		}
		reconcileTenantCert(t, k8s, o)
		if got := certDNS(t, o.Namespace, o.Name); !slices.Equal(got, want) {
			t.Errorf("%s: dnsNames = %v, want %v", name, got, want)
		}
	}
}

func TestTenantCertCap(t *testing.T) {
	requireEnvtest(t)
	o := tcOptions("tc-cap", "wildcard-apps")
	o.MaxNames = 2
	createTestCert(t, o.Namespace, o.Name, "*."+testBase)
	for _, n := range []string{"tc-cap-c", "tc-cap-a", "tc-cap-b"} {
		createBareTN(t, n)
	}
	reconcileTenantCert(t, k8s, o)
	live := liveTNs(t)
	slices.SortFunc(live, func(a, b TenantNamespace) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	kept := []string{live[0].Name, live[1].Name}
	slices.Sort(kept)
	want := []string{"*." + testBase, tenantName(kept[0]), tenantName(kept[1])}
	if got := certDNS(t, o.Namespace, o.Name); !slices.Equal(got, want) {
		t.Errorf("dnsNames = %v, want the two oldest: %v", got, want)
	}
	// Deterministic: a second pass changes nothing.
	rv := getCert(t, o.Namespace, o.Name).GetResourceVersion()
	reconcileTenantCert(t, k8s, o)
	if getCert(t, o.Namespace, o.Name).GetResourceVersion() != rv {
		t.Error("capped list is not stable")
	}
}

func setCertConditions(t *testing.T, ns, name string, conds ...[2]string) {
	t.Helper()
	c := getCert(t, ns, name)
	var list []any
	for _, cd := range conds {
		list = append(list, map[string]any{"type": cd[0], "status": cd[1], "lastTransitionTime": metav1.Now().UTC().Format(time.RFC3339), "reason": "Test", "message": "test"})
	}
	if err := unstructured.SetNestedSlice(c.Object, list, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Status().Update(ctx, c); err != nil {
		t.Fatal(err)
	}
}

func TestTenantCertDefersWhileNotReady(t *testing.T) {
	requireEnvtest(t)
	o := tcOptions("tc-defer", "wildcard-apps")
	createTestCert(t, o.Namespace, o.Name, "*."+testBase)
	createBareTN(t, "tc-defer-a")

	for name, conds := range map[string][][2]string{
		"not ready": {{"Ready", "False"}},
		"issuing":   {{"Ready", "True"}, {"Issuing", "True"}},
	} {
		setCertConditions(t, o.Namespace, o.Name, conds...)
		rv := getCert(t, o.Namespace, o.Name).GetResourceVersion()
		if res := reconcileTenantCert(t, k8s, o); res.RequeueAfter != time.Minute {
			t.Errorf("%s: RequeueAfter = %v", name, res.RequeueAfter)
		}
		if getCert(t, o.Namespace, o.Name).GetResourceVersion() != rv {
			t.Errorf("%s: certificate was patched", name)
		}
	}

	// Changes made while deferred are coalesced into one patch once it is Ready.
	createBareTN(t, "tc-defer-b")
	setCertConditions(t, o.Namespace, o.Name, [2]string{"Ready", "False"})
	reconcileTenantCert(t, k8s, o)
	if got := certDNS(t, o.Namespace, o.Name); len(got) != 1 {
		t.Fatalf("patched while not ready: %v", got)
	}
	setCertConditions(t, o.Namespace, o.Name, [2]string{"Ready", "True"})
	if res := reconcileTenantCert(t, k8s, o); res.RequeueAfter != 0 {
		t.Errorf("ready: RequeueAfter = %v", res.RequeueAfter)
	}
	got := certDNS(t, o.Namespace, o.Name)
	for _, n := range []string{"tc-defer-a", "tc-defer-b"} {
		if !slices.Contains(got, tenantName(n)) {
			t.Errorf("%s missing after coalescing: %v", n, got)
		}
	}
}

// A TeamNamespace whose deletion starts while the Certificate is absent is not
// listed once the Certificate appears.
func TestTenantCertDeletionStartsWhileAbsent(t *testing.T) {
	requireEnvtest(t)
	ensurePlainNamespace(t, "tc-absent-del")
	o := tcOptions("tc-absent-del", "wildcard-apps")
	createBareTN(t, "tc-absent-del-x", "test.ithaca-idp.com/hold")
	if res := reconcileTenantCert(t, k8s, o); res.RequeueAfter != time.Minute {
		t.Fatalf("RequeueAfter = %v", res.RequeueAfter)
	}
	if err := k8s.Delete(ctx, newTN("tc-absent-del-x")); err != nil {
		t.Fatal(err)
	}
	createTestCert(t, o.Namespace, o.Name, "*."+testBase)
	reconcileTenantCert(t, k8s, o)
	if slices.Contains(certDNS(t, o.Namespace, o.Name), tenantName("tc-absent-del-x")) {
		t.Error("namespace being deleted is listed")
	}
}

func TestTenantCertAbsentIsNeverCreated(t *testing.T) {
	requireEnvtest(t)
	ensurePlainNamespace(t, "tc-absent")
	o := tcOptions("tc-absent", "wildcard-apps")
	createBareTN(t, "tc-absent-a")
	res := reconcileTenantCert(t, k8s, o)
	if res.RequeueAfter != time.Minute {
		t.Errorf("RequeueAfter = %v", res.RequeueAfter)
	}
	err := k8s.Get(ctx, types.NamespacedName{Namespace: o.Namespace, Name: o.Name}, newUnstructured(CertificateGVK, "", ""))
	if !apierrors.IsNotFound(err) {
		t.Errorf("certificate was created or the lookup failed: %v", err)
	}
}

func TestTenantCertCRDCheck(t *testing.T) {
	requireEnvtest(t)
	if missing, err := TenantCertCRDMissing(k8s.RESTMapper()); err != nil || missing {
		t.Errorf("Certificate CRD reported missing=%v err=%v", missing, err)
	}
}

// The manager wiring: TeamNamespace events and drift on the Certificate reach
// the reconciler through the namespace-restricted cache.
func TestTenantCertControllerWatches(t *testing.T) {
	requireEnvtest(t)
	o := tcOptions("tc-watch", "wildcard-apps")
	createTestCert(t, o.Namespace, o.Name, "*."+testBase)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  testScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			newUnstructured(CertificateGVK, "", ""): {Namespaces: map[string]cache.Config{o.Namespace: {}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&TenantCertReconciler{Client: mgr.GetClient(), Options: o}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	mctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(mctx) }()
	t.Cleanup(func() { cancel(); <-done })

	createBareTN(t, "tc-watch-a")
	eventually(t, "name added on TeamNamespace create", func() error {
		if !slices.Contains(certDNS(t, o.Namespace, o.Name), tenantName("tc-watch-a")) {
			return fmt.Errorf("not yet")
		}
		return nil
	})
	c := getCert(t, o.Namespace, o.Name)
	_ = unstructured.SetNestedStringSlice(c.Object, []string{"*." + testBase}, "spec", "dnsNames")
	if err := k8s.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	eventually(t, "drift repaired", func() error {
		if !slices.Contains(certDNS(t, o.Namespace, o.Name), tenantName("tc-watch-a")) {
			return fmt.Errorf("not yet")
		}
		return nil
	})
	if err := k8s.Delete(ctx, newTN("tc-watch-a")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "name removed on TeamNamespace delete", func() error {
		if slices.Contains(certDNS(t, o.Namespace, o.Name), tenantName("tc-watch-a")) {
			return fmt.Errorf("still there")
		}
		return nil
	})
}

// --- admission policy ---

const (
	policyCertNS   = "istio-ingress"
	policyCertName = "wildcard-apps"
)

// tenantCertFixture gives the operator update rights on certificates in two
// namespaces without resourceNames, on purpose: every denial below must then
// come from the admission policy, not from RBAC (the narrow Role the
// deployment uses is in idp-gitops).
func tenantCertFixture(t *testing.T) client.Client {
	t.Helper()
	op := operatorClient(t)
	for _, ns := range []string{policyCertNS, "tc-other"} {
		ensurePlainNamespace(t, ns)
		for _, o := range []client.Object{
			&rbacv1.Role{
				ObjectMeta: metav1.ObjectMeta{Name: "tc-test", Namespace: ns},
				Rules: []rbacv1.PolicyRule{{
					APIGroups: []string{"cert-manager.io"}, Resources: []string{"certificates", "certificates/status"},
					Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"},
				}},
			},
			&rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "tc-test", Namespace: ns},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "tc-test"},
				Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: operatorUser}},
			},
		} {
			if err := k8s.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []struct{ ns, name string }{{policyCertNS, policyCertName}, {policyCertNS, "other-cert"}, {"tc-other", policyCertName}} {
		u := newUnstructured(CertificateGVK, c.ns, c.name)
		err := k8s.Get(ctx, client.ObjectKeyFromObject(u), u)
		if apierrors.IsNotFound(err) {
			if err := k8s.Create(ctx, testCert(c.ns, c.name, "*."+testBase)); err != nil {
				t.Fatal(err)
			}
		} else if err != nil {
			t.Fatal(err)
		} else {
			resetCert(t, c.ns, c.name)
		}
	}
	// RBAC has to be visible to the apiserver before the first call.
	eventually(t, "operator may update certificates", func() error {
		c := getCert(t, policyCertNS, policyCertName)
		return op.Update(ctx, c, client.DryRunAll)
	})
	return op
}

func resetCert(t *testing.T, ns, name string) {
	t.Helper()
	c := getCert(t, ns, name)
	c.SetLabels(nil)
	c.SetAnnotations(nil)
	c.SetFinalizers(nil)
	c.Object["spec"] = testCert(ns, name, "*."+testBase).Object["spec"]
	if err := k8s.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
}

func opUpdateCert(op client.Client, ns, name string, dry bool, mutate func(u *unstructured.Unstructured)) error {
	u := newUnstructured(CertificateGVK, ns, name)
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(u), u); err != nil {
		return err
	}
	mutate(u)
	if dry {
		return op.Update(ctx, u, client.DryRunAll)
	}
	return op.Update(ctx, u)
}

func setDNS(names ...string) func(u *unstructured.Unstructured) {
	return func(u *unstructured.Unstructured) {
		_ = unstructured.SetNestedStringSlice(u.Object, names, "spec", "dnsNames")
	}
}

func manyNames(n int) []string {
	out := []string{"*." + testBase}
	for i := 0; i < n; i++ {
		out = append(out, tenantName(fmt.Sprintf("t%03d", i)))
	}
	return out
}

func TestPolicyTenantCertAllowsOnlyDNSNames(t *testing.T) {
	op := tenantCertFixture(t)
	w := "*." + testBase

	// Add and remove names (persisted, so the stored object is checked too).
	if err := opUpdateCert(op, policyCertNS, policyCertName, false, setDNS(w, tenantName("team-a"), tenantName("team-b"))); err != nil {
		t.Fatalf("add names: %v", err)
	}
	if got := certDNS(t, policyCertNS, policyCertName); len(got) != 3 {
		t.Errorf("dnsNames = %v", got)
	}
	if err := opUpdateCert(op, policyCertNS, policyCertName, false, setDNS(w, tenantName("team-b"))); err != nil {
		t.Fatalf("remove a name: %v", err)
	}
	if err := opUpdateCert(op, policyCertNS, policyCertName, true, setDNS(w)); err != nil {
		t.Errorf("only the wildcard: %v", err)
	}
	if err := opUpdateCert(op, policyCertNS, policyCertName, true, setDNS(manyNames(99)...)); err != nil {
		t.Errorf("100 entries (the cap): %v", err)
	}
	if err := opUpdateCert(op, policyCertNS, policyCertName, true, setDNS(w, tenantName(strings.Repeat("a", 40)))); err != nil {
		t.Errorf("a 40 character label: %v", err)
	}
	// A merge patch of dnsNames only, as the controller sends it.
	patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"dnsNames":["*.`+testBase+`","*.team-c.`+testBase+`"]}}`))
	if err := op.Patch(ctx, newUnstructured(CertificateGVK, policyCertNS, policyCertName), patch, client.DryRunAll); err != nil {
		t.Errorf("merge patch: %v", err)
	}
}

func TestPolicyTenantCertDenials(t *testing.T) {
	op := tenantCertFixture(t)
	w := "*." + testBase
	dns := func(names ...string) func(u *unstructured.Unstructured) { return setDNS(names...) }
	set := func(v any, path ...string) func(u *unstructured.Unstructured) {
		return func(u *unstructured.Unstructured) { _ = unstructured.SetNestedField(u.Object, v, path...) }
	}

	for name, mutate := range map[string]func(u *unstructured.Unstructured){
		"other host":            dns(w, "*.evil.com"),
		"two labels":            dns(w, "*.a.b."+testBase),
		"bare base":             dns(w, testBase),
		"bare namespace":        dns(w, "team-a."+testBase),
		"reserved vault":        dns(w, tenantName("vault")),
		"reserved spinnaker":    dns(w, tenantName("spinnaker")),
		"reserved keycloak":     dns(w, tenantName("keycloak")),
		"reserved vault prefix": dns(w, tenantName("vault-x")),
		"reserved kube prefix":  dns(w, tenantName("kube-system")),
		"reserved istio prefix": dns(w, tenantName("istio-system")),
		"reserved gke prefix":   dns(w, tenantName("gke-x")),
		"reserved default":      dns(w, tenantName("default")),
		"reserved argocd":       dns(w, tenantName("argocd")),
		"reserved cert-manager": dns(w, tenantName("cert-manager")),
		"reserved idp-system":   dns(w, tenantName("idp-system")),
		"label over 40":         dns(w, tenantName(strings.Repeat("a", 41))),
		"uppercase":             dns(w, "*.Team-A."+testBase),
		"trailing dash":         dns(w, tenantName("team-")),
		"not a wildcard":        dns(w, "team-a."+"b."+testBase),
		"101 entries":           dns(manyNames(100)...),
		"duplicates":            dns(w, tenantName("team-a"), tenantName("team-a")),
		"duplicate wildcard":    dns(w, w),
		"reordered":             dns(tenantName("team-a"), w),
		"wildcard missing":      dns(tenantName("team-a")),
		"empty":                 dns(),
		"issuerRef":             set("evil", "spec", "issuerRef", "name"),
		"issuerRef kind":        set("Issuer", "spec", "issuerRef", "kind"),
		"secretName":            set("other-tls", "spec", "secretName"),
		"commonName":            set("x.evil.com", "spec", "commonName"),
		"duration":              set("1h", "spec", "duration"),
		"label": func(u *unstructured.Unstructured) {
			u.SetLabels(map[string]string{"a": "b"})
		},
		"annotation": func(u *unstructured.Unstructured) {
			u.SetAnnotations(map[string]string{"a": "b"})
		},
		"finalizer": func(u *unstructured.Unstructured) {
			u.SetFinalizers([]string{"example.com/x"})
		},
		"ownerReference": func(u *unstructured.Unstructured) {
			u.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "x", UID: "1"}})
		},
		"new field privateKey": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "Always", "spec", "privateKey", "rotationPolicy")
		},
		"new field uris": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedStringSlice(u.Object, []string{"spiffe://evil/x"}, "spec", "uris")
		},
		"removed spec field": func(u *unstructured.Unstructured) {
			unstructured.RemoveNestedField(u.Object, "spec", "duration")
		},
	} {
		wantPolicyDenial(t, name, opUpdateCert(op, policyCertNS, policyCertName, true, mutate))
	}

	// The status subresource is not writable for the operator.
	st := getCert(t, policyCertNS, policyCertName)
	_ = unstructured.SetNestedField(st.Object, int64(7), "status", "revision")
	wantPolicyDenial(t, "status subresource", op.Status().Update(ctx, st, client.DryRunAll))

	// Same change that is allowed on the right object, on the wrong ones.
	wantPolicyDenial(t, "another name", opUpdateCert(op, policyCertNS, "other-cert", true, dns(w, tenantName("team-a"))))
	wantPolicyDenial(t, "another namespace", opUpdateCert(op, "tc-other", policyCertName, true, dns(w, tenantName("team-a"))))

	// Create and delete.
	wantPolicyDenial(t, "create", op.Create(ctx, testCert(policyCertNS, "new-cert", w), client.DryRunAll))
	wantPolicyDenial(t, "create in another namespace", op.Create(ctx, testCert("tc-other", "new-cert", w), client.DryRunAll))
	wantPolicyDenial(t, "delete", op.Delete(ctx, newUnstructured(CertificateGVK, policyCertNS, policyCertName), client.DryRunAll))
	wantPolicyDenial(t, "delete other", op.Delete(ctx, newUnstructured(CertificateGVK, policyCertNS, "other-cert"), client.DryRunAll))

	// A merge patch that also touches issuerRef.
	patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"issuerRef":{"name":"evil"},"dnsNames":["*.`+testBase+`"]}}`))
	wantPolicyDenial(t, "patch issuerRef", op.Patch(ctx, newUnstructured(CertificateGVK, policyCertNS, policyCertName), patch, client.DryRunAll))

	// Nothing above was persisted.
	if got := certDNS(t, policyCertNS, policyCertName); !slices.Equal(got, []string{w}) {
		t.Errorf("dnsNames = %v", got)
	}
}

// The real reconciler as the operator ServiceAccount, against the real policy.
func TestTenantCertReconcilerUnderPolicy(t *testing.T) {
	op := tenantCertFixture(t)
	createBareTN(t, "tc-policy-a")
	o := tcOptions(policyCertNS, policyCertName)
	reconcileTenantCert(t, op, o)
	want, _, _ := o.DesiredDNSNames(liveTNs(t))
	if got := certDNS(t, policyCertNS, policyCertName); !slices.Equal(got, want) || !slices.Contains(got, tenantName("tc-policy-a")) {
		t.Errorf("dnsNames = %v, want %v", got, want)
	}
}
