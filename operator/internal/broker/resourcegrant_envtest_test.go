package broker

import (
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

func grantReconciler(c Cloud) *ResourceGrantReconciler {
	r := NewResourceGrantReconciler(k8s, testConfig(c))
	r.Now = func() time.Time { return testNow }
	return r
}

func newGrant(ns, name, workload, resource, access string) *idpv1alpha1.ResourceGrant {
	return &idpv1alpha1.ResourceGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       idpv1alpha1.ResourceGrantSpec{Workload: workload, Resource: resource, Access: access},
	}
}

func reconcileGrant(t *testing.T, r *ResourceGrantReconciler, ns, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	if err != nil {
		t.Fatalf("reconcile grant %s/%s: %v", ns, name, err)
	}
	return res
}

func grantReady(t *testing.T, ns, name string) *metav1.Condition {
	t.Helper()
	return readyOf(get(t, &idpv1alpha1.ResourceGrant{}, ns, name).Status.Conditions)
}

func newWI(t *testing.T, ns, name string) {
	t.Helper()
	if err := k8s.Create(ctx, &idpv1alpha1.WorkloadIdentity{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}); err != nil {
		t.Fatal(err)
	}
}

// readyClaim creates a claim, marks it Ready as the claim controller would and
// seeds its bucket in the fake.
func readyClaim(t *testing.T, f *fakeCloud, ns, name string) *fakeBucket {
	t.Helper()
	bc := newClaim(ns, name)
	if err := k8s.Create(ctx, bc); err != nil {
		t.Fatal(err)
	}
	bc.Status.BucketName = bn(ns, name)
	meta.SetStatusCondition(&bc.Status.Conditions, readyCondition(bc.Generation, metav1.ConditionTrue, ReasonProvisioned, "test"))
	if err := k8s.Status().Update(ctx, bc); err != nil {
		t.Fatal(err)
	}
	return f.seed(bc.Status.BucketName, OwnerLabels(ns, name, string(bc.UID)), 0)
}

func TestResourceGrantWaitsForWorkloadThenBinds(t *testing.T) {
	requireEnvtest(t)
	ns := "rg-wait"
	managedNamespace(t, ns)
	f := newFakeCloud()
	b := readyClaim(t, f, ns, "data")
	r := grantReconciler(f)
	if err := k8s.Create(ctx, newGrant(ns, "web-read", "web", "data", "reader")); err != nil {
		t.Fatal(err)
	}

	res := reconcileGrant(t, r, ns, "web-read")
	wantReady(t, grantReady(t, ns, "web-read"), metav1.ConditionFalse, ReasonWorkloadNotFound)
	if res.RequeueAfter == 0 || len(b.bindings) != 0 {
		t.Errorf("requeue %v, bindings %v", res.RequeueAfter, b.bindings)
	}

	newWI(t, ns, "web")
	reconcileGrant(t, r, ns, "web-read")
	wantReady(t, grantReady(t, ns, "web-read"), metav1.ConditionTrue, ReasonBound)
	if got := b.members(RoleReader); len(got) != 1 || got[0] != memberOf(ns, "web") {
		t.Errorf("reader members = %v", got)
	}
	rg := get(t, &idpv1alpha1.ResourceGrant{}, ns, "web-read")
	if rg.Status.Member != memberOf(ns, "web") || rg.Status.ObservedGeneration != rg.Generation {
		t.Errorf("status = %+v", rg.Status)
	}
	if len(rg.Finalizers) != 1 || rg.Finalizers[0] != GrantFinalizerName {
		t.Errorf("finalizers = %v", rg.Finalizers)
	}

	// Reconciling again writes nothing.
	w := b.writes
	reconcileGrant(t, r, ns, "web-read")
	if b.writes != w {
		t.Error("a no-op reconcile wrote the policy")
	}
}

func TestResourceGrantReaderAndWriter(t *testing.T) {
	requireEnvtest(t)
	ns := "rg-roles"
	managedNamespace(t, ns)
	f := newFakeCloud()
	b := readyClaim(t, f, ns, "data")
	b.bindings = []Binding{{Role: "roles/storage.admin", Members: []string{"user:boss@example.com"}}}
	newWI(t, ns, "web")
	newWI(t, ns, "api")
	r := grantReconciler(f)
	for _, g := range []*idpv1alpha1.ResourceGrant{
		newGrant(ns, "g1", "web", "data", "reader"), newGrant(ns, "g2", "api", "data", "writer"),
	} {
		if err := k8s.Create(ctx, g); err != nil {
			t.Fatal(err)
		}
		reconcileGrant(t, r, ns, g.Name)
	}
	if got := b.members(RoleReader); len(got) != 1 || got[0] != memberOf(ns, "web") {
		t.Errorf("readers = %v", got)
	}
	if got := b.members(RoleWriter); len(got) != 1 || got[0] != memberOf(ns, "api") {
		t.Errorf("writers = %v", got)
	}
	if got := b.members("roles/storage.admin"); len(got) != 1 || got[0] != "user:boss@example.com" {
		t.Errorf("foreign binding = %v", got)
	}
	for _, bind := range b.bindings {
		if bind.Role != "roles/storage.admin" && bind.Role != RoleReader && bind.Role != RoleWriter {
			t.Errorf("role %q written", bind.Role)
		}
	}
}

func TestResourceGrantDeleteRemovesBinding(t *testing.T) {
	requireEnvtest(t)
	ns := "rg-del"
	managedNamespace(t, ns)
	f := newFakeCloud()
	b := readyClaim(t, f, ns, "data")
	newWI(t, ns, "web")
	newWI(t, ns, "api")
	r := grantReconciler(f)
	for _, g := range []*idpv1alpha1.ResourceGrant{
		newGrant(ns, "g1", "web", "data", "reader"), newGrant(ns, "g2", "api", "reader", "reader"),
	} {
		if g.Spec.Resource == "reader" {
			g.Spec.Resource = "data"
		}
		if err := k8s.Create(ctx, g); err != nil {
			t.Fatal(err)
		}
		reconcileGrant(t, r, ns, g.Name)
	}
	if len(b.members(RoleReader)) != 2 {
		t.Fatalf("readers = %v", b.members(RoleReader))
	}
	if err := k8s.Delete(ctx, newGrant(ns, "g1", "web", "data", "reader")); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, r, ns, "g1")
	if got := b.members(RoleReader); len(got) != 1 || got[0] != memberOf(ns, "api") {
		t.Errorf("readers after delete = %v", got)
	}
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "g1"}, &idpv1alpha1.ResourceGrant{}); !apierrors.IsNotFound(err) {
		t.Errorf("grant still present: %v", err)
	}
}

func TestResourceGrantDeleteRetriesOnCloudError(t *testing.T) {
	requireEnvtest(t)
	ns := "rg-del-retry"
	managedNamespace(t, ns)
	f := newFakeCloud()
	b := readyClaim(t, f, ns, "data")
	newWI(t, ns, "web")
	r := grantReconciler(f)
	if err := k8s.Create(ctx, newGrant(ns, "g1", "web", "data", "writer")); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, r, ns, "g1")
	if err := k8s.Delete(ctx, newGrant(ns, "g1", "web", "data", "writer")); err != nil {
		t.Fatal(err)
	}
	f.fail("update", errors.New("iam unavailable"))
	res := reconcileGrant(t, r, ns, "g1")
	if res.RequeueAfter == 0 || len(b.members(RoleWriter)) != 1 {
		t.Fatalf("res %+v writers %v", res, b.members(RoleWriter))
	}
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "g1"}, &idpv1alpha1.ResourceGrant{}); err != nil {
		t.Fatalf("grant gone while its binding remains: %v", err)
	}
	// The namespace is still live, so even after the give-up time it keeps trying.
	r.Now = func() time.Time { return time.Now().Add(3 * time.Hour) }
	f.fail("update", errors.New("iam unavailable"))
	reconcileGrant(t, r, ns, "g1")
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "g1"}, &idpv1alpha1.ResourceGrant{}); err != nil {
		t.Fatalf("finalizer dropped in a live namespace: %v", err)
	}
	reconcileGrant(t, r, ns, "g1")
	if len(b.members(RoleWriter)) != 0 {
		t.Error("binding not removed")
	}
}

func TestResourceGrantRepairsDriftAndLeavesForeignBindings(t *testing.T) {
	requireEnvtest(t)
	ns := "rg-drift"
	managedNamespace(t, ns)
	f := newFakeCloud()
	b := readyClaim(t, f, ns, "data")
	newWI(t, ns, "web")
	r := grantReconciler(f)
	if err := k8s.Create(ctx, newGrant(ns, "g1", "web", "data", "writer")); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, r, ns, "g1")

	// Someone removes our binding and adds a stray one for a workload without
	// grant, a binding for another namespace, and a foreign one.
	b.bindings = []Binding{
		{Role: "roles/storage.admin", Members: []string{"user:boss@example.com"}},
		{Role: RoleWriter, Members: []string{memberOf(ns, "stray"), memberOf("other-ns", "web")}},
		{Role: "roles/storage.objectAdmin", Members: []string{memberOf(ns, "web")}},
	}
	reconcileGrant(t, r, ns, "g1")
	if got := b.members(RoleWriter); len(got) != 2 || got[0] != memberOf("other-ns", "web") || got[1] != memberOf(ns, "web") {
		t.Errorf("writers = %v", got)
	}
	if len(b.members("roles/storage.admin")) != 1 || len(b.members("roles/storage.objectAdmin")) != 1 {
		t.Errorf("foreign bindings changed: %+v", b.bindings)
	}
}

func TestResourceGrantRevokesWhenWorkloadGoes(t *testing.T) {
	requireEnvtest(t)
	ns := "rg-wi-gone"
	managedNamespace(t, ns)
	f := newFakeCloud()
	b := readyClaim(t, f, ns, "data")
	newWI(t, ns, "web")
	r := grantReconciler(f)
	if err := k8s.Create(ctx, newGrant(ns, "g1", "web", "data", "reader")); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, r, ns, "g1")
	if err := k8s.Delete(ctx, &idpv1alpha1.WorkloadIdentity{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"}}); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, r, ns, "g1")
	wantReady(t, grantReady(t, ns, "g1"), metav1.ConditionFalse, ReasonWorkloadNotFound)
	if len(b.members(RoleReader)) != 0 {
		t.Errorf("readers = %v", b.members(RoleReader))
	}
}

func TestResourceGrantClaimStates(t *testing.T) {
	requireEnvtest(t)
	ns := "rg-claim"
	managedNamespace(t, ns)
	f := newFakeCloud()
	newWI(t, ns, "web")
	r := grantReconciler(f)

	if err := k8s.Create(ctx, newGrant(ns, "g1", "web", "nope", "reader")); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, r, ns, "g1")
	wantReady(t, grantReady(t, ns, "g1"), metav1.ConditionFalse, ReasonClaimNotFound)

	// A claim that is not Ready yet.
	if err := k8s.Create(ctx, newClaim(ns, "fresh")); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(ctx, newGrant(ns, "g2", "web", "fresh", "reader")); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, r, ns, "g2")
	wantReady(t, grantReady(t, ns, "g2"), metav1.ConditionFalse, ReasonClaimNotReady)
	if len(f.calls) != 0 {
		t.Errorf("cloud calls: %v", f.calls)
	}
	if len(get(t, &idpv1alpha1.ResourceGrant{}, ns, "g2").Finalizers) != 0 {
		t.Error("finalizer added before there was anything to bind")
	}
}

func TestResourceGrantNeverTouchesForeignBucket(t *testing.T) {
	requireEnvtest(t)
	ns := "rg-foreign"
	managedNamespace(t, ns)
	f := newFakeCloud()
	b := readyClaim(t, f, ns, "data")
	b.labels = map[string]string{LabelManaged: "true", LabelNamespace: "intruder", LabelClaim: "data"}
	newWI(t, ns, "web")
	r := grantReconciler(f)
	if err := k8s.Create(ctx, newGrant(ns, "g1", "web", "data", "reader")); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, r, ns, "g1")
	wantReady(t, grantReady(t, ns, "g1"), metav1.ConditionFalse, ReasonClaimNotReady)
	noMutation(t, f)

	// Deleting such a grant does not touch it either.
	if err := k8s.Delete(ctx, newGrant(ns, "g1", "web", "data", "reader")); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, r, ns, "g1")
	noMutation(t, f)
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "g1"}, &idpv1alpha1.ResourceGrant{}); !apierrors.IsNotFound(err) {
		t.Errorf("grant stuck: %v", err)
	}
}

func TestResourceGrantNotManagedNamespace(t *testing.T) {
	requireEnvtest(t)
	f := newFakeCloud()
	ns := "kube-system"
	if err := k8s.Create(ctx, newGrant(ns, "g1", "web", "data", "reader")); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, grantReconciler(f), ns, "g1")
	wantReady(t, grantReady(t, ns, "g1"), metav1.ConditionFalse, ReasonNotManaged)
	if len(f.calls) != 0 {
		t.Errorf("cloud calls: %v", f.calls)
	}
}

func TestResourceGrantSurvivesEtagConflicts(t *testing.T) {
	requireEnvtest(t)
	ns := "rg-etag"
	managedNamespace(t, ns)
	f := newFakeCloud()
	b := readyClaim(t, f, ns, "data")
	newWI(t, ns, "web")
	f.conflicts = 3
	if err := k8s.Create(ctx, newGrant(ns, "g1", "web", "data", "reader")); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, grantReconciler(f), ns, "g1")
	wantReady(t, grantReady(t, ns, "g1"), metav1.ConditionTrue, ReasonBound)
	if len(b.members(RoleReader)) != 1 {
		t.Errorf("readers = %v", b.members(RoleReader))
	}
}

func TestResourceGrantCloudErrorIsRetried(t *testing.T) {
	requireEnvtest(t)
	ns := "rg-err"
	managedNamespace(t, ns)
	f := newFakeCloud()
	readyClaim(t, f, ns, "data")
	newWI(t, ns, "web")
	f.fail("update", errors.New("boom"))
	if err := k8s.Create(ctx, newGrant(ns, "g1", "web", "data", "reader")); err != nil {
		t.Fatal(err)
	}
	r := grantReconciler(f)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "g1"}}); err == nil {
		t.Fatal("cloud error swallowed")
	}
	wantReady(t, grantReady(t, ns, "g1"), metav1.ConditionFalse, ReasonGrantFailed)
	reconcileGrant(t, r, ns, "g1")
	wantReady(t, grantReady(t, ns, "g1"), metav1.ConditionTrue, ReasonBound)
}

func TestResourceGrantBrokerDisabled(t *testing.T) {
	requireEnvtest(t)
	ns := "rg-off"
	managedNamespace(t, ns)
	if err := k8s.Create(ctx, newGrant(ns, "g1", "web", "data", "reader")); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, grantReconciler(nil), ns, "g1")
	rg := get(t, &idpv1alpha1.ResourceGrant{}, ns, "g1")
	if len(rg.Status.Conditions) != 0 || len(rg.Finalizers) != 0 {
		t.Errorf("disabled broker touched the grant: %+v %v", rg.Status, rg.Finalizers)
	}
}

// Claim deletion removes every binding and the grants then find no claim.
func TestClaimDeleteThenGrantsReportClaimNotFound(t *testing.T) {
	requireEnvtest(t)
	ns := "rg-claim-del"
	managedNamespace(t, ns)
	f := newFakeCloud()
	cr := claimReconciler(f)
	if err := k8s.Create(ctx, newClaim(ns, "data")); err != nil {
		t.Fatal(err)
	}
	reconcileClaim(t, cr, ns, "data")
	newWI(t, ns, "web")
	gr := grantReconciler(f)
	if err := k8s.Create(ctx, newGrant(ns, "g1", "web", "data", "reader")); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, gr, ns, "g1")
	wantReady(t, grantReady(t, ns, "g1"), metav1.ConditionTrue, ReasonBound)
	deleteClaim(t, ns, "data")
	reconcileClaim(t, cr, ns, "data")
	reconcileGrant(t, gr, ns, "g1")
	wantReady(t, grantReady(t, ns, "g1"), metav1.ConditionFalse, ReasonClaimNotFound)
}

// Another project's bucket with every label copied (including the claim UID).
func TestResourceGrantRefusesBucketOfAnotherProject(t *testing.T) {
	requireEnvtest(t)
	ns := "rg-proj"
	managedNamespace(t, ns)
	f := newFakeCloud()
	b := readyClaim(t, f, ns, "data")
	b.project = "999999999999"
	newWI(t, ns, "web")
	r := grantReconciler(f)
	if err := k8s.Create(ctx, newGrant(ns, "g1", "web", "data", "reader")); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, r, ns, "g1")
	wantReady(t, grantReady(t, ns, "g1"), metav1.ConditionFalse, ReasonClaimNotReady)
	noMutation(t, f)
	if len(b.bindings) != 0 {
		t.Errorf("bindings = %v", b.bindings)
	}
}

func TestResourceGrantRefusesReleasingAndOrphanedBuckets(t *testing.T) {
	requireEnvtest(t)
	ns := "rg-rel"
	managedNamespace(t, ns)
	f := newFakeCloud()
	b := readyClaim(t, f, ns, "data")
	newWI(t, ns, "web")
	newWI(t, ns, "api")
	r := grantReconciler(f)
	for _, g := range []*idpv1alpha1.ResourceGrant{newGrant(ns, "g1", "web", "data", "reader"), newGrant(ns, "g2", "api", "data", "reader")} {
		if err := k8s.Create(ctx, g); err != nil {
			t.Fatal(err)
		}
		reconcileGrant(t, r, ns, g.Name)
	}
	if len(b.members(RoleReader)) != 2 {
		t.Fatalf("readers = %v", b.members(RoleReader))
	}
	for name, labels := range map[string]map[string]string{"releasing": ReleasingLabels(), "orphaned": OrphanLabels(testNow)} {
		for k, v := range labels {
			b.labels[k] = v
		}
		before := f.callsTo("update")
		reconcileGrant(t, r, ns, "g1")
		wantReady(t, grantReady(t, ns, "g1"), metav1.ConditionFalse, ReasonClaimNotReady)
		if f.callsTo("update") != before {
			t.Errorf("%s: a grant wrote to a bucket that is being retired", name)
		}
	}

	// Grants of a retired bucket can still be deleted: nothing to remove, no wedge.
	for _, n := range []string{"g1", "g2"} {
		if err := k8s.Delete(ctx, newGrant(ns, n, "x", "data", "reader")); err != nil {
			t.Fatal(err)
		}
		reconcileGrant(t, r, ns, n)
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: n}, &idpv1alpha1.ResourceGrant{}); !apierrors.IsNotFound(err) {
			t.Errorf("grant %s stuck: %v", n, err)
		}
	}

	// And with the bucket gone altogether.
	f2 := newFakeCloud()
	readyClaim(t, f2, "rg-rel", "logs")
	delete(f2.buckets, bn("rg-rel", "logs"))
	r2 := grantReconciler(f2)
	if err := k8s.Create(ctx, newGrant(ns, "g3", "web", "logs", "reader")); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, r2, ns, "g3")
	if err := k8s.Delete(ctx, newGrant(ns, "g3", "web", "logs", "reader")); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, r2, ns, "g3")
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "g3"}, &idpv1alpha1.ResourceGrant{}); !apierrors.IsNotFound(err) {
		t.Errorf("grant of a missing bucket stuck: %v", err)
	}
}

func managedLeft(b *fakeBucket, ns string) []string {
	scope := testScope
	scope.Namespace = ns
	var out []string
	for _, bind := range b.bindings {
		for _, m := range bind.Members {
			if scope.Manages(bind.Role, m) {
				out = append(out, bind.Role+" "+m)
			}
		}
	}
	return out
}

// A grant sync that had already passed its checks writes after the claim's
// release stripped the bindings: the post-write check strips them again, and
// nothing is left that a tenant could use.
func TestReleaseVersusGrantSyncRace(t *testing.T) {
	requireEnvtest(t)
	ns := "rg-race"
	managedNamespace(t, ns)
	f := newFakeCloud()
	cr := claimReconciler(f)
	gr := grantReconciler(f)
	mkClaim(t, ns, "data")
	reconcileClaim(t, cr, ns, "data")
	b := f.bucket(bn(ns, "data"))
	b.objects = 3
	newWI(t, ns, "web")
	newWI(t, ns, "api")
	if err := k8s.Create(ctx, newGrant(ns, "g1", "web", "data", "reader")); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, gr, ns, "g1")
	if err := k8s.Create(ctx, newGrant(ns, "g2", "api", "data", "writer")); err != nil {
		t.Fatal(err)
	}

	// Just before g2's policy write, the claim is deleted and fully released.
	f.onNext("update", func() {
		deleteClaim(t, ns, "data")
		reconcileClaim(t, cr, ns, "data")
	})
	reconcileGrant(t, gr, ns, "g2")
	if b.labels[LabelOrphaned] != "true" {
		t.Fatalf("labels = %v", b.labels)
	}
	if left := managedLeft(b, ns); len(left) != 0 {
		t.Errorf("tenant bindings survived the release: %v", left)
	}
	wantReady(t, grantReady(t, ns, "g2"), metav1.ConditionFalse, ReasonClaimNotReady)

	// The grants are not wedged: deleting them succeeds.
	for _, n := range []string{"g1", "g2"} {
		if err := k8s.Delete(ctx, newGrant(ns, n, "x", "data", "reader")); err != nil {
			t.Fatal(err)
		}
		reconcileGrant(t, gr, ns, n)
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: n}, &idpv1alpha1.ResourceGrant{}); !apierrors.IsNotFound(err) {
			t.Errorf("grant %s stuck: %v", n, err)
		}
	}
}

// A grant reconcile that starts in the middle of a release (after the releasing
// label, before the bindings are stripped) refuses and writes nothing.
func TestGrantSyncDuringReleaseWritesNothing(t *testing.T) {
	requireEnvtest(t)
	ns := "rg-race2"
	managedNamespace(t, ns)
	f := newFakeCloud()
	cr := claimReconciler(f)
	gr := grantReconciler(f)
	mkClaim(t, ns, "data")
	reconcileClaim(t, cr, ns, "data")
	b := f.bucket(bn(ns, "data"))
	b.objects = 1
	newWI(t, ns, "web")
	if err := k8s.Create(ctx, newGrant(ns, "g1", "web", "data", "reader")); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, gr, ns, "g1")

	f.onNext("update", func() { // first strip of the release, label already set
		if b.labels[LabelReleasing] != "true" {
			t.Error("releasing label not set before the bindings are stripped")
		}
		writes := b.writes
		reconcileGrant(t, gr, ns, "g1")
		if b.writes != writes {
			t.Error("a grant wrote during the release")
		}
	})
	deleteClaim(t, ns, "data")
	reconcileClaim(t, cr, ns, "data")
	if left := managedLeft(b, ns); len(left) != 0 {
		t.Errorf("bindings left: %v", left)
	}
	if b.labels[LabelOrphaned] != "true" {
		t.Errorf("labels = %v", b.labels)
	}
}

// TestSameClaimNameInTwoNamespacesKeepsSeparateBuckets: claims, grants and workloads of the same
// names in two namespaces get different buckets, and each bucket binds only its own namespace's
// principal.
func TestSameClaimNameInTwoNamespacesKeepsSeparateBuckets(t *testing.T) {
	requireEnvtest(t)
	f := newFakeCloud()
	r := grantReconciler(f)
	buckets := map[string]*fakeBucket{}
	for _, ns := range []string{"rg-twin-a", "rg-twin-b"} {
		managedNamespace(t, ns)
		buckets[ns] = readyClaim(t, f, ns, "data")
		newWI(t, ns, "web")
		if err := k8s.Create(ctx, newGrant(ns, "web-data", "web", "data", "reader")); err != nil {
			t.Fatal(err)
		}
		reconcileGrant(t, r, ns, "web-data")
	}
	if bn("rg-twin-a", "data") == bn("rg-twin-b", "data") {
		t.Fatal("two namespaces share a bucket name")
	}
	for ns, b := range buckets {
		if got := b.members(RoleReader); len(got) != 1 || got[0] != memberOf(ns, "web") {
			t.Errorf("%s readers = %v", ns, got)
		}
	}
	// revoking in one namespace leaves the other's binding alone
	g := get(t, &idpv1alpha1.ResourceGrant{}, "rg-twin-a", "web-data")
	if err := k8s.Delete(ctx, g); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, r, "rg-twin-a", "web-data")
	if got := buckets["rg-twin-a"].members(RoleReader); len(got) != 0 {
		t.Errorf("a revoked binding remains: %v", got)
	}
	if got := buckets["rg-twin-b"].members(RoleReader); len(got) != 1 {
		t.Errorf("the other namespace lost its binding: %v", got)
	}
}
