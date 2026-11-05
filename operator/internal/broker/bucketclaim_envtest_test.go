package broker

import (
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

const (
	testProjectID     = "proj-1"
	testProjectNumber = "123456789"
)

var testNow = time.Unix(1700000000, 0)

func testConfig(c Cloud) Config {
	return Config{Cloud: c, ProjectID: testProjectID, ProjectNumber: testProjectNumber, GiveUpAfter: time.Hour}
}

func claimReconciler(c Cloud) *BucketClaimReconciler {
	r := NewBucketClaimReconciler(k8s, testConfig(c))
	r.Now = func() time.Time { return testNow }
	return r
}

func newClaim(ns, name string) *idpv1alpha1.BucketClaim {
	return &idpv1alpha1.BucketClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

func reconcileClaim(t *testing.T, r *BucketClaimReconciler, ns, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	if err != nil {
		t.Fatalf("reconcile claim %s/%s: %v", ns, name, err)
	}
	return res
}

func readyOf(conds []metav1.Condition) *metav1.Condition {
	return meta.FindStatusCondition(conds, idpv1alpha1.ConditionReady)
}

func claimReady(t *testing.T, ns, name string) *metav1.Condition {
	t.Helper()
	return readyOf(get(t, &idpv1alpha1.BucketClaim{}, ns, name).Status.Conditions)
}

func wantReady(t *testing.T, c *metav1.Condition, status metav1.ConditionStatus, reason string) {
	t.Helper()
	if c == nil || c.Status != status || c.Reason != reason {
		t.Errorf("Ready = %+v, want %s/%s", c, status, reason)
	}
}

// noMutation fails if the cloud saw any call that could change a bucket.
func noMutation(t *testing.T, f *fakeCloud) {
	t.Helper()
	for _, op := range []string{"create", "delete", "labels", "update"} {
		if n := f.callsTo(op); n != 0 {
			t.Errorf("%d %s calls on a bucket that must not be touched", n, op)
		}
	}
}

// bn is the bucket of a claim in the test project.
func bn(ns, claim string) string {
	n, err := BucketName(testProjectNumber, ns, claim)
	if err != nil {
		panic(err)
	}
	return n
}

// mkClaim creates a claim and returns it with its UID.
func mkClaim(t *testing.T, ns, name string) *idpv1alpha1.BucketClaim {
	t.Helper()
	bc := newClaim(ns, name)
	if err := k8s.Create(ctx, bc); err != nil {
		t.Fatal(err)
	}
	return bc
}

func TestBucketClaimCreatesBucket(t *testing.T) {
	requireEnvtest(t)
	ns := "bc-create"
	managedNamespace(t, ns)
	f := newFakeCloud()
	r := claimReconciler(f)
	claim := mkClaim(t, ns, "data")
	reconcileClaim(t, r, ns, "data")

	b := f.bucket(bn(ns, "data"))
	if b == nil {
		t.Fatal("bucket not created")
	}
	want := OwnerLabels(ns, "data", string(claim.UID))
	if len(b.labels) != 4 || b.labels[LabelManaged] != "true" || b.labels[LabelNamespace] != ns || b.labels[LabelClaim] != "data" ||
		b.labels[LabelClaimUID] != want[LabelClaimUID] || want[LabelClaimUID] == "" {
		t.Errorf("labels = %v", b.labels)
	}
	bc := get(t, &idpv1alpha1.BucketClaim{}, ns, "data")
	if bc.Status.BucketName != bn(ns, "data") || bc.Status.ObservedGeneration != bc.Generation {
		t.Errorf("status = %+v", bc.Status)
	}
	wantReady(t, readyOf(bc.Status.Conditions), metav1.ConditionTrue, ReasonProvisioned)
	if len(bc.Finalizers) != 1 || bc.Finalizers[0] != BucketFinalizerName {
		t.Errorf("finalizers = %v", bc.Finalizers)
	}

	// A second reconcile creates nothing.
	reconcileClaim(t, r, ns, "data")
	if n := f.callsTo("create"); n != 1 {
		t.Errorf("create calls = %d", n)
	}
}

func TestBucketClaimAdoptsOwnBucket(t *testing.T) {
	requireEnvtest(t)
	ns := "bc-adopt"
	managedNamespace(t, ns)
	f := newFakeCloud()
	claim := mkClaim(t, ns, "data")
	f.seed(bn(ns, "data"), OwnerLabels(ns, "data", string(claim.UID)), 3)
	reconcileClaim(t, claimReconciler(f), ns, "data")
	wantReady(t, claimReady(t, ns, "data"), metav1.ConditionTrue, ReasonProvisioned)
	noMutation(t, f)
}

// A crash between create and the status write: the create then reports
// "exists" and the labels decide.
func TestBucketClaimAdoptsAfterCreateRace(t *testing.T) {
	requireEnvtest(t)
	ns := "bc-race"
	managedNamespace(t, ns)
	f := newFakeCloud()
	claim := mkClaim(t, ns, "data")
	// The first read misses the bucket; the create then says it exists.
	f.blindGets = 1
	f.seed(bn(ns, "data"), OwnerLabels(ns, "data", string(claim.UID)), 0)
	reconcileClaim(t, claimReconciler(f), ns, "data")
	if f.callsTo("labels")+f.callsTo("delete") != 0 {
		t.Errorf("calls: %v", f.calls)
	}
	wantReady(t, claimReady(t, ns, "data"), metav1.ConditionTrue, ReasonProvisioned)
}

func TestBucketClaimNameTaken(t *testing.T) {
	requireEnvtest(t)
	ns := "bc-taken"
	managedNamespace(t, ns)
	admin := []Binding{{Role: "roles/storage.admin", Members: []string{"user:a@example.com"}}}
	for _, name := range []string{"nolabels", "other", "notours", "unmanaged", "orph", "olduid", "nouid", "otherproj", "old"} {
		f := newFakeCloud()
		claim := mkClaim(t, ns, name)
		uid := string(claim.UID)
		labels := map[string]map[string]string{
			"nolabels":  nil,
			"other":     {LabelManaged: "true", LabelNamespace: "somewhere-else", LabelClaim: "other", LabelClaimUID: uid},
			"notours":   {LabelManaged: "false", LabelNamespace: ns, LabelClaim: name, LabelClaimUID: uid},
			"unmanaged": {LabelNamespace: ns, LabelClaim: name, LabelClaimUID: uid},
			"orph":      OwnerLabels(ns, name, uid),
			// An earlier incarnation of the claim, and a bucket with no UID.
			"olduid":    OwnerLabels(ns, name, "11111111-1111-1111-1111-111111111111"),
			"nouid":     {LabelManaged: "true", LabelNamespace: ns, LabelClaim: name},
			"otherproj": OwnerLabels(ns, name, uid),
			"old":       OwnerLabels(ns, name, uid),
		}[name]
		b := f.seed(bn(ns, name), labels, 5)
		b.bindings = cloneBindings(admin)
		switch name {
		case "orph":
			for k, v := range OrphanLabels(testNow) {
				b.labels[k] = v
			}
		case "otherproj":
			// Someone else's bucket with every label copied: the project decides.
			b.project = "999999999999"
		case "old":
			// Not created by this incarnation of the claim.
			b.created = claim.CreationTimestamp.Add(-time.Hour)
		}
		res := reconcileClaim(t, claimReconciler(f), ns, name)
		wantReady(t, claimReady(t, ns, name), metav1.ConditionFalse, ReasonNameTaken)
		if res.RequeueAfter == 0 {
			t.Errorf("%s: no requeue", name)
		}
		if bc := get(t, &idpv1alpha1.BucketClaim{}, ns, name); bc.Status.BucketName != "" {
			t.Errorf("%s: bucketName = %q", name, bc.Status.BucketName)
		}
		noMutation(t, f)
		if !SameBindings(b.bindings, admin) {
			t.Errorf("%s: bindings changed", name)
		}
	}

	// A bucket Google will not show (another project's).
	f := newFakeCloud()
	f.forbidden[bn(ns, "theirs")] = true
	mkClaim(t, ns, "theirs")
	reconcileClaim(t, claimReconciler(f), ns, "theirs")
	wantReady(t, claimReady(t, ns, "theirs"), metav1.ConditionFalse, ReasonNameTaken)
	noMutation(t, f)

	// Create says "exists" and the bucket turns out to be foreign.
	f = newFakeCloud()
	f.blindGets = 1 // the first GetBucket misses it
	mkClaim(t, ns, "late")
	f.seed(bn(ns, "late"), map[string]string{"x": "y"}, 0)
	reconcileClaim(t, claimReconciler(f), ns, "late")
	wantReady(t, claimReady(t, ns, "late"), metav1.ConditionFalse, ReasonNameTaken)
	// The create was attempted and refused; nothing else happened.
	if f.callsTo("delete")+f.callsTo("labels")+f.callsTo("update") != 0 {
		t.Errorf("calls: %v", f.calls)
	}
}

func TestBucketClaimInvalidNames(t *testing.T) {
	requireEnvtest(t)
	// 32 characters: a valid TeamNamespace name that cannot own buckets.
	ns := "bc-namespace-of-thirty-two-chars"
	if len(ns) != 32 {
		t.Fatalf("test namespace has %d characters", len(ns))
	}
	managedNamespace(t, ns)
	f := newFakeCloud()
	mkClaim(t, ns, "data")
	reconcileClaim(t, claimReconciler(f), ns, "data")
	wantReady(t, claimReady(t, ns, "data"), metav1.ConditionFalse, ReasonInvalidName)
	if c := claimReady(t, ns, "data"); !strings.Contains(c.Message, "31") {
		t.Errorf("message = %q", c.Message)
	}
	ns2 := "bc-names"
	managedNamespace(t, ns2)
	for _, n := range []string{"google-data", "my-google", "goog"} {
		mkClaim(t, ns2, n)
		reconcileClaim(t, claimReconciler(f), ns2, n)
		wantReady(t, claimReady(t, ns2, n), metav1.ConditionFalse, ReasonInvalidName)
	}
	if len(f.calls) != 0 {
		t.Errorf("cloud calls for names Google would refuse: %v", f.calls)
	}
}

func TestBucketClaimCreateFailureIsRetried(t *testing.T) {
	requireEnvtest(t)
	ns := "bc-fail"
	managedNamespace(t, ns)
	f := newFakeCloud()
	f.fail("create", errors.New("quota exceeded"))
	if err := k8s.Create(ctx, newClaim(ns, "data")); err != nil {
		t.Fatal(err)
	}
	r := claimReconciler(f)
	nn := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "data"}}
	if _, err := r.Reconcile(ctx, nn); err == nil {
		t.Fatal("create failure was swallowed")
	}
	wantReady(t, claimReady(t, ns, "data"), metav1.ConditionFalse, ReasonCreateFailed)
	reconcileClaim(t, r, ns, "data")
	wantReady(t, claimReady(t, ns, "data"), metav1.ConditionTrue, ReasonProvisioned)
}

func TestBucketClaimIgnoredOutsideManagedNamespaces(t *testing.T) {
	requireEnvtest(t)
	f := newFakeCloud()
	plain := "bc-plain"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: plain, Labels: map[string]string{"owner": "someone-else"}}}); err != nil {
		t.Fatal(err)
	}
	fake := "bc-fake"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fake, Labels: map[string]string{
		"app.kubernetes.io/managed-by": "idp-operator", "idp.ithaca-idp.com/team-namespace": fake}}}); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{plain, fake, "kube-system"} {
		if err := k8s.Create(ctx, newClaim(ns, "data")); err != nil {
			t.Fatal(err)
		}
		reconcileClaim(t, claimReconciler(f), ns, "data")
		wantReady(t, claimReady(t, ns, "data"), metav1.ConditionFalse, ReasonNamespaceNotManaged)
		if len(get(t, &idpv1alpha1.BucketClaim{}, ns, "data").Finalizers) != 0 {
			t.Errorf("%s: finalizer added", ns)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("cloud calls: %v", f.calls)
	}
}

func TestBucketClaimBrokerDisabled(t *testing.T) {
	requireEnvtest(t)
	ns := "bc-off"
	managedNamespace(t, ns)
	if err := k8s.Create(ctx, newClaim(ns, "data")); err != nil {
		t.Fatal(err)
	}
	reconcileClaim(t, claimReconciler(nil), ns, "data")
	bc := get(t, &idpv1alpha1.BucketClaim{}, ns, "data")
	if len(bc.Status.Conditions) != 0 || len(bc.Finalizers) != 0 {
		t.Errorf("disabled broker touched the claim: %+v %v", bc.Status, bc.Finalizers)
	}
}

func deleteClaim(t *testing.T, ns, name string) {
	t.Helper()
	if err := k8s.Delete(ctx, newClaim(ns, name)); err != nil {
		t.Fatal(err)
	}
}

func claimGone(t *testing.T, ns, name string) bool {
	t.Helper()
	err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &idpv1alpha1.BucketClaim{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err != nil
}

func seedBindings(b *fakeBucket, ns string) {
	b.bindings = []Binding{
		{Role: "roles/storage.admin", Members: []string{"user:boss@example.com"}},
		{Role: RoleReader, Members: []string{memberOf(ns, "web"), memberOf("other-ns", "web")}},
		{Role: RoleWriter, Members: []string{memberOf(ns, "api")}},
	}
}

func TestBucketClaimDeleteEmptyBucketDeletesIt(t *testing.T) {
	requireEnvtest(t)
	ns := "bc-del-empty"
	managedNamespace(t, ns)
	f := newFakeCloud()
	r := claimReconciler(f)
	mkClaim(t, ns, "data")
	reconcileClaim(t, r, ns, "data")
	seedBindings(f.bucket(bn(ns, "data")), ns)
	deleteClaim(t, ns, "data")
	reconcileClaim(t, r, ns, "data")
	if f.bucket(bn(ns, "data")) != nil {
		t.Error("empty bucket not deleted")
	}
	if !claimGone(t, ns, "data") {
		t.Error("finalizer not removed")
	}
	if f.callsTo("empty") != 0 {
		t.Error("emptiness was checked by listing objects")
	}
}

func TestBucketClaimDeleteNonEmptyBucketOrphansIt(t *testing.T) {
	requireEnvtest(t)
	ns := "bc-del-full"
	managedNamespace(t, ns)
	f := newFakeCloud()
	r := claimReconciler(f)
	claim := mkClaim(t, ns, "data")
	reconcileClaim(t, r, ns, "data")
	b := f.bucket(bn(ns, "data"))
	b.objects = 7
	seedBindings(b, ns)
	deleteClaim(t, ns, "data")
	reconcileClaim(t, r, ns, "data")

	if f.bucket(bn(ns, "data")) == nil {
		t.Fatal("a bucket with objects was deleted")
	}
	if b.labels[LabelOrphaned] != "true" || b.labels[LabelOrphanedAt] != "1700000000" || b.labels[LabelManaged] != "true" ||
		b.labels[LabelReleasing] != "true" {
		t.Errorf("labels = %v", b.labels)
	}
	if got := b.members(RoleReader); len(got) != 1 || got[0] != memberOf("other-ns", "web") {
		t.Errorf("reader members = %v", got)
	}
	if got := b.members(RoleWriter); len(got) != 0 {
		t.Errorf("writer members = %v", got)
	}
	if got := b.members("roles/storage.admin"); len(got) != 1 {
		t.Errorf("foreign binding changed: %v", got)
	}
	if !claimGone(t, ns, "data") {
		t.Error("an orphaned bucket blocked the finalizer")
	}

	// The same claim again does not adopt the retired bucket.
	if err := k8s.Create(ctx, newClaim(ns, "data")); err != nil {
		t.Fatal(err)
	}
	reconcileClaim(t, r, ns, "data")
	wantReady(t, claimReady(t, ns, "data"), metav1.ConditionFalse, ReasonNameTaken)
	if string(get(t, &idpv1alpha1.BucketClaim{}, ns, "data").UID) == string(claim.UID) {
		t.Error("expected a new UID")
	}
}

// The bucket gains an object between the claim's labelling and the delete.
func TestBucketClaimDeleteRaceOrphans(t *testing.T) {
	requireEnvtest(t)
	ns := "bc-del-race"
	managedNamespace(t, ns)
	f := newFakeCloud()
	r := claimReconciler(f)
	mkClaim(t, ns, "data")
	reconcileClaim(t, r, ns, "data")
	f.fail("delete", ErrNotEmpty)
	deleteClaim(t, ns, "data")
	reconcileClaim(t, r, ns, "data")
	b := f.bucket(bn(ns, "data"))
	if b == nil || b.labels[LabelOrphaned] != "true" {
		t.Errorf("bucket = %+v", b)
	}
	if !claimGone(t, ns, "data") {
		t.Error("finalizer stuck")
	}
}

// A conflict on the metageneration (the bucket changed after the read that
// verified it) makes the release re-read and re-verify.
func TestBucketClaimDeleteReverifiesOnConflict(t *testing.T) {
	requireEnvtest(t)
	ns := "bc-del-conf"
	managedNamespace(t, ns)
	f := newFakeCloud()
	r := claimReconciler(f)
	mkClaim(t, ns, "data")
	reconcileClaim(t, r, ns, "data")
	b := f.bucket(bn(ns, "data"))
	// Just before the first label write somebody else changes the bucket.
	f.onNext("labels", func() { f.mu.Lock(); b.metagen++; f.mu.Unlock() })
	deleteClaim(t, ns, "data")
	reconcileClaim(t, r, ns, "data")
	if f.bucket(bn(ns, "data")) != nil || !claimGone(t, ns, "data") {
		t.Error("release did not recover from a metageneration conflict")
	}
	if f.callsTo("get") < 3 {
		t.Errorf("bucket was not re-read: %v", f.calls)
	}

	// If, meanwhile, the labels were replaced by a stranger's, nothing is touched.
	mkClaim(t, ns, "other")
	reconcileClaim(t, r, ns, "other")
	b = f.bucket(bn(ns, "other"))
	seedBindings(b, ns)
	before := cloneBindings(b.bindings)
	f.onNext("labels", func() {
		f.mu.Lock()
		b.metagen++
		b.labels = map[string]string{LabelManaged: "true", LabelNamespace: "intruder", LabelClaim: "other"}
		f.mu.Unlock()
	})
	deleteClaim(t, ns, "other")
	reconcileClaim(t, r, ns, "other")
	if !claimGone(t, ns, "other") || f.bucket(bn(ns, "other")) == nil || !SameBindings(before, b.bindings) {
		t.Error("a bucket that changed hands was modified")
	}
}

func TestBucketClaimDeleteNeverTouchesForeignBucket(t *testing.T) {
	requireEnvtest(t)
	ns := "bc-del-foreign"
	managedNamespace(t, ns)
	f := newFakeCloud()
	r := claimReconciler(f)
	claim := mkClaim(t, ns, "data")
	reconcileClaim(t, r, ns, "data")
	// Someone relabels the bucket (or it is replaced by another project's).
	b := f.bucket(bn(ns, "data"))
	b.labels = map[string]string{LabelManaged: "true", LabelNamespace: "intruder", LabelClaim: "data", LabelClaimUID: string(claim.UID)}
	seedBindings(b, ns)
	before := cloneBindings(b.bindings)
	deleteClaim(t, ns, "data")
	reconcileClaim(t, r, ns, "data")
	if !claimGone(t, ns, "data") {
		t.Error("finalizer stuck")
	}
	if f.bucket(bn(ns, "data")) == nil || !SameBindings(before, b.bindings) {
		t.Error("foreign bucket was changed")
	}
	if f.callsTo("delete") != 0 || f.callsTo("labels") != 0 || f.callsTo("update") != 0 {
		t.Errorf("mutating calls: %v", f.calls)
	}

	// Another project's bucket with every label copied (same name, same UID).
	f3 := newFakeCloud()
	r3 := claimReconciler(f3)
	claim = mkClaim(t, ns, "copy")
	reconcileClaim(t, r3, ns, "copy")
	b = f3.bucket(bn(ns, "copy"))
	b.project = "999999999999"
	seedBindings(b, ns)
	before = cloneBindings(b.bindings)
	deleteClaim(t, ns, "copy")
	reconcileClaim(t, r3, ns, "copy")
	if !claimGone(t, ns, "copy") || f3.bucket(bn(ns, "copy")) == nil || !SameBindings(before, b.bindings) {
		t.Error("a bucket of another project was changed")
	}
	if f3.callsTo("delete")+f3.callsTo("labels")+f3.callsTo("update") != 0 {
		t.Errorf("mutating calls: %v", f3.calls)
	}

	// A bucket owned by another project that Google will not show.
	f2 := newFakeCloud()
	r2 := claimReconciler(f2)
	mkClaim(t, ns, "other")
	reconcileClaim(t, r2, ns, "other")
	f2.forbidden[bn(ns, "other")] = true
	deleteClaim(t, ns, "other")
	reconcileClaim(t, r2, ns, "other")
	if !claimGone(t, ns, "other") {
		t.Error("finalizer stuck")
	}
	if f2.callsTo("delete")+f2.callsTo("labels")+f2.callsTo("update") != 0 {
		t.Errorf("mutating calls: %v", f2.calls)
	}
}

func TestBucketClaimDeleteRetriesThenGivesUp(t *testing.T) {
	requireEnvtest(t)
	ns := "bc-del-retry"
	managedNamespace(t, ns)
	f := newFakeCloud()
	r := claimReconciler(f)
	mkClaim(t, ns, "data")
	reconcileClaim(t, r, ns, "data")
	b := f.bucket(bn(ns, "data"))
	seedBindings(b, ns)
	deleteClaim(t, ns, "data")

	boom := errors.New("iam unavailable")
	f.fail("update", boom, boom)
	res := reconcileClaim(t, r, ns, "data")
	if claimGone(t, ns, "data") || res.RequeueAfter == 0 {
		t.Fatalf("finalizer dropped on a cloud failure (res %+v)", res)
	}
	wantReady(t, claimReady(t, ns, "data"), metav1.ConditionFalse, ReasonTerminating)

	// Past the give-up time the finalizer goes anyway, but the bucket is
	// retired first (orphan labels, bindings stripped) as far as Google answers.
	r.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	reconcileClaim(t, r, ns, "data")
	if !claimGone(t, ns, "data") {
		t.Error("a failing cloud held the finalizer past the give-up time")
	}
	if b.labels[LabelOrphaned] != "true" || len(b.members(RoleWriter)) != 0 || len(b.members(RoleReader)) != 1 {
		t.Errorf("give-up left the bucket live: labels %v bindings %+v", b.labels, b.bindings)
	}
}

// After a give-up nothing re-adopts the bucket: not when it was retired, and not
// when Google was down for the retiring too (the claim UID differs).
func TestBucketClaimNeverAdoptsAfterGiveUp(t *testing.T) {
	requireEnvtest(t)
	ns := "bc-giveup"
	managedNamespace(t, ns)
	f := newFakeCloud()
	r := claimReconciler(f)
	mkClaim(t, ns, "data")
	reconcileClaim(t, r, ns, "data")
	b := f.bucket(bn(ns, "data"))
	b.objects = 1
	seedBindings(b, ns)
	deleteClaim(t, ns, "data")
	// Google is unreachable for the whole release, including the forced retiring.
	boom := errors.New("google unavailable")
	f.fail("get", boom, boom, boom, boom, boom, boom, boom, boom, boom, boom)
	r.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	reconcileClaim(t, r, ns, "data")
	if !claimGone(t, ns, "data") {
		t.Fatal("finalizer stuck")
	}
	if b.labels[LabelOrphaned] == "true" {
		t.Fatal("test expects the retiring to have failed too")
	}
	f.failures["get"] = nil

	// A new claim of the same name finds an Owned bucket with another UID.
	mkClaim(t, ns, "data")
	reconcileClaim(t, claimReconciler(f), ns, "data")
	wantReady(t, claimReady(t, ns, "data"), metav1.ConditionFalse, ReasonNameTaken)
	if f.callsTo("create") != 1 || f.callsTo("delete") != 0 {
		t.Errorf("calls: %v", f.calls)
	}
	if len(b.members(RoleWriter)) != 1 {
		t.Error("the new claim changed the old bucket")
	}
}
