package delivery

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

const (
	goodRepo = "https://github.com/ithaca-lab-org/sample-orders-api"
	goodSHA  = "0123456789abcdef0123456789abcdef01234567"
)

var secretErr = &GateError{Status: 503, Reason: "Service Unavailable"}

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := idpv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&idpv1alpha1.DeliveryPipeline{}, &idpv1alpha1.DeliveryRun{}).Build()
}

func workload(ns, name string) *idpv1alpha1.WorkloadIdentity {
	return &idpv1alpha1.WorkloadIdentity{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

func pipelineObj(repo string) *idpv1alpha1.DeliveryPipeline {
	return &idpv1alpha1.DeliveryPipeline{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo", Name: "orders", Generation: 1},
		Spec:       idpv1alpha1.DeliveryPipelineSpec{Workload: "orders", Repo: repo},
	}
}

func runObj(name, ref string) *idpv1alpha1.DeliveryRun {
	return &idpv1alpha1.DeliveryRun{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo", Name: name, CreationTimestamp: metav1.Now()},
		Spec:       idpv1alpha1.DeliveryRunSpec{Pipeline: "orders", Ref: ref, RequestedBy: "sub-1"},
	}
}

func pipelineRec(c client.Client, g Gate) *PipelineReconciler {
	return NewPipelineReconciler(c, Config{Gate: g, AllowedOrgs: []string{"ithaca-lab-org"}})
}

func runRec(c client.Client, g Gate) *RunReconciler {
	return NewRunReconciler(c, Config{Gate: g, AllowedOrgs: []string{"ithaca-lab-org"}})
}

func reconcilePipeline(t *testing.T, r *PipelineReconciler, c client.Client) (ctrl.Result, *idpv1alpha1.DeliveryPipeline) {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "demo", Name: "orders"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	dp := &idpv1alpha1.DeliveryPipeline{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "demo", Name: "orders"}, dp); err != nil {
		t.Fatal(err)
	}
	return res, dp
}

func reconcileRun(t *testing.T, r *RunReconciler, c client.Client, name string) (ctrl.Result, *idpv1alpha1.DeliveryRun) {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "demo", Name: name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	run := &idpv1alpha1.DeliveryRun{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "demo", Name: name}, run); err != nil {
		t.Fatal(err)
	}
	return res, run
}

func TestPipelineSuccess(t *testing.T) {
	c := newClient(t, pipelineObj(goodRepo), workload("demo", "orders"))
	g := newFakeGate()
	r := pipelineRec(c, g)

	// The application is created first (an asynchronous Spinnaker task).
	res, dp := reconcilePipeline(t, r, c)
	if dp.Status.Phase != idpv1alpha1.DeliveryPhaseBuilding || res.RequeueAfter == 0 {
		t.Fatalf("after create: %s, %+v", dp.Status.Phase, res)
	}
	if _, ok := g.pending["demo-orders"]; !ok {
		t.Fatal("application not created")
	}
	// Then the pipeline, and only READY once Gate lists it.
	res, dp = reconcilePipeline(t, r, c)
	if dp.Status.Phase != idpv1alpha1.DeliveryPhaseReady || dp.Status.ApplicationName != "demo-orders" || res.RequeueAfter != driftResync {
		t.Fatalf("after pipeline: %+v, %+v", dp.Status, res)
	}
	if !strings.Contains(string(g.Pipelines["demo-orders"]), `"reference": "`+goodRepo+`"`) {
		t.Errorf("pipeline not saved from the template: %s", g.Pipelines["demo-orders"])
	}
	if g.Apps["demo-orders"] != AppDescription("demo", "orders") {
		t.Errorf("description = %q", g.Apps["demo-orders"])
	}
	if dp.Status.ObservedGeneration != 1 || len(dp.Status.Conditions) != 1 || dp.Status.Conditions[0].Status != metav1.ConditionTrue {
		t.Errorf("status = %+v", dp.Status)
	}

	// Up to date: a resync only checks, it does not save again.
	g.Calls = nil
	reconcilePipeline(t, r, c)
	for _, call := range g.Calls {
		if strings.HasPrefix(call, "save:") || strings.HasPrefix(call, "create:") {
			t.Errorf("unexpected write on resync: %v", g.Calls)
		}
	}
	// A pipeline that vanished from Spinnaker is put back.
	delete(g.Pipelines, "demo-orders")
	reconcilePipeline(t, r, c)
	if _, ok := g.Pipelines["demo-orders"]; !ok {
		t.Error("missing pipeline not recreated")
	}
}

func TestPipelineUpdatedOnSpecChange(t *testing.T) {
	c := newClient(t, pipelineObj(goodRepo), workload("demo", "orders"))
	g := newFakeGate()
	r := pipelineRec(c, g)
	reconcilePipeline(t, r, c)
	reconcilePipeline(t, r, c)

	dp := &idpv1alpha1.DeliveryPipeline{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "demo", Name: "orders"}, dp)
	dp.Spec.Path = "k8s/prod"
	dp.Generation = 2
	if err := c.Update(context.Background(), dp); err != nil {
		t.Fatal(err)
	}
	reconcilePipeline(t, r, c)
	if !strings.Contains(string(g.Pipelines["demo-orders"]), "k8s/prod/kustomization.yaml") {
		t.Errorf("pipeline not updated: %s", g.Pipelines["demo-orders"])
	}
	found := false
	for _, call := range g.Calls {
		found = found || call == "save:pid-demo-orders"
	}
	if !found {
		t.Errorf("expected an update with the existing id: %v", g.Calls)
	}
}

func TestPipelineNotReadyUntilGateListsIt(t *testing.T) {
	c := newClient(t, pipelineObj(goodRepo), workload("demo", "orders"))
	g := newFakeGate()
	g.HidePipeline = true
	r := pipelineRec(c, g)
	reconcilePipeline(t, r, c)
	res, dp := reconcilePipeline(t, r, c)
	if dp.Status.Phase != idpv1alpha1.DeliveryPhaseBuilding || res.RequeueAfter != pipelineWait {
		t.Errorf("phase = %s, %+v", dp.Status.Phase, res)
	}
}

func TestPipelineGateDown(t *testing.T) {
	c := newClient(t, pipelineObj(goodRepo), workload("demo", "orders"))
	g := newFakeGate()
	g.Err = secretErr
	r := pipelineRec(c, g)
	res, dp := reconcilePipeline(t, r, c)
	if dp.Status.Phase != idpv1alpha1.DeliveryPhaseBuilding || res.RequeueAfter != errorRetry {
		t.Fatalf("phase = %s, %+v", dp.Status.Phase, res)
	}
	if dp.Status.Message != "gate: HTTP 503 (Service Unavailable)" || dp.Status.Conditions[0].Reason != ReasonGateUnavailable {
		t.Errorf("status = %+v", dp.Status)
	}
	// Gate comes back: it recovers without intervention.
	g.Err = nil
	reconcilePipeline(t, r, c)
	_, dp = reconcilePipeline(t, r, c)
	if dp.Status.Phase != idpv1alpha1.DeliveryPhaseReady {
		t.Errorf("phase = %s", dp.Status.Phase)
	}
	// A READY pipeline stays READY while Gate flaps.
	g.Err = &GateError{Reason: "unreachable"}
	_, dp = reconcilePipeline(t, r, c)
	if dp.Status.Phase != idpv1alpha1.DeliveryPhaseReady {
		t.Errorf("READY downgraded to %s", dp.Status.Phase)
	}
}

func TestPipelineGateRejects(t *testing.T) {
	c := newClient(t, pipelineObj(goodRepo), workload("demo", "orders"))
	g := newFakeGate()
	g.Err = &GateError{Status: 400, Reason: "Bad Request"}
	_, dp := reconcilePipeline(t, pipelineRec(c, g), c)
	if dp.Status.Conditions[0].Reason != ReasonGateRejected {
		t.Errorf("status = %+v", dp.Status)
	}
}

func TestPipelineWorkloadMissing(t *testing.T) {
	c := newClient(t, pipelineObj(goodRepo))
	g := newFakeGate()
	res, dp := reconcilePipeline(t, pipelineRec(c, g), c)
	if dp.Status.Phase != idpv1alpha1.DeliveryPhaseFailed || !strings.Contains(dp.Status.Message, `WorkloadIdentity "orders" does not exist`) || res.RequeueAfter == 0 {
		t.Errorf("status = %+v, %+v", dp.Status, res)
	}
	if len(g.Calls) != 0 {
		t.Errorf("Gate called without a workload: %v", g.Calls)
	}
	// The workload shows up: the pipeline leaves FAILED.
	if err := c.Create(context.Background(), workload("demo", "orders")); err != nil {
		t.Fatal(err)
	}
	pipelineRec(c, g).Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "demo", Name: "orders"}})
	_, dp = reconcilePipeline(t, pipelineRec(c, g), c)
	if dp.Status.Phase != idpv1alpha1.DeliveryPhaseReady {
		t.Errorf("phase = %s (%s)", dp.Status.Phase, dp.Status.Message)
	}
}

func TestPipelineRefusals(t *testing.T) {
	for name, repo := range map[string]string{
		"disallowed org": "https://github.com/evil-org/app",
		"not https":      "http://github.com/ithaca-lab-org/app",
		"other host":     "https://example.com/ithaca-lab-org/app",
	} {
		c := newClient(t, pipelineObj(repo), workload("demo", "orders"))
		g := newFakeGate()
		res, dp := reconcilePipeline(t, pipelineRec(c, g), c)
		if dp.Status.Phase != idpv1alpha1.DeliveryPhaseFailed || dp.Status.Conditions[0].Reason != ReasonInvalidSpec || res.RequeueAfter != 0 {
			t.Errorf("%s: status = %+v, %+v", name, dp.Status, res)
		}
		if len(g.Calls) != 0 {
			t.Errorf("%s: Gate called: %v", name, g.Calls)
		}
	}
	dp := pipelineObj(goodRepo)
	dp.Spec.Path = "../../etc"
	c := newClient(t, dp, workload("demo", "orders"))
	if _, got := reconcilePipeline(t, pipelineRec(c, newFakeGate()), c); got.Status.Phase != idpv1alpha1.DeliveryPhaseFailed {
		t.Errorf("path traversal accepted: %+v", got.Status)
	}
}

func TestPipelineApplicationConflict(t *testing.T) {
	c := newClient(t, pipelineObj(goodRepo), workload("demo", "orders"))
	g := newFakeGate()
	// a-b + c and a + b-c can join to the same name; the marker of another pair is never adopted.
	g.Apps["demo-orders"] = AppDescription("demo-orders", "x")
	_, dp := reconcilePipeline(t, pipelineRec(c, g), c)
	if dp.Status.Phase != idpv1alpha1.DeliveryPhaseFailed || dp.Status.Conditions[0].Reason != ReasonAppConflict {
		t.Errorf("status = %+v", dp.Status)
	}
	if _, ok := g.Pipelines["demo-orders"]; ok {
		t.Error("pipeline written into a foreign application")
	}
	// An application without a marker (made in Deck or by a script) is refused: never adopted,
	// never stamped, its pipeline untouched.
	g2 := newFakeGate()
	g2.Apps["demo-orders"] = ""
	c2 := newClient(t, pipelineObj(goodRepo), workload("demo", "orders"))
	_, dp = reconcilePipeline(t, pipelineRec(c2, g2), c2)
	if dp.Status.Phase != idpv1alpha1.DeliveryPhaseFailed || dp.Status.Conditions[0].Reason != ReasonAppConflict ||
		!strings.Contains(dp.Status.Message, "not managed by idp") || len(g2.Pipelines) != 0 || g2.Apps["demo-orders"] != "" || len(g2.pending) != 0 {
		t.Errorf("unmarked application not refused: %+v", dp.Status)
	}
	// Collision: namespace "a-b" + workload "c" and namespace "a" + workload "b-c" both want
	// "a-b-c". Whichever created it first keeps it; the other pair is refused, never adopted.
	g3 := newFakeGate()
	first := collisionPipeline("a-b", "c")
	c3 := newClient(t, first, workload("a-b", "c"))
	r3 := pipelineRec(c3, g3)
	reconcileOne(t, r3, "a-b", "c")
	reconcileOne(t, r3, "a-b", "c")
	if g3.Apps["a-b-c"] != AppDescription("a-b", "c") {
		t.Fatalf("marker = %q", g3.Apps["a-b-c"])
	}
	second := collisionPipeline("a", "b-c")
	if err := c3.Create(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err := c3.Create(context.Background(), workload("a", "b-c")); err != nil {
		t.Fatal(err)
	}
	before := string(g3.Pipelines["a-b-c"])
	reconcileOne(t, r3, "a", "b-c")
	got := &idpv1alpha1.DeliveryPipeline{}
	_ = c3.Get(context.Background(), types.NamespacedName{Namespace: "a", Name: "b-c"}, got)
	if got.Status.Phase != idpv1alpha1.DeliveryPhaseFailed || got.Status.Conditions[0].Reason != ReasonAppConflict || string(g3.Pipelines["a-b-c"]) != before {
		t.Errorf("second pair adopted the application: %+v", got.Status)
	}
}

func TestDisabledIsNoOp(t *testing.T) {
	c := newClient(t, pipelineObj(goodRepo), workload("demo", "orders"), runObj("r1", goodSHA))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "demo", Name: "orders"}}
	if res, err := NewPipelineReconciler(c, Config{}).Reconcile(context.Background(), req); err != nil || res != (ctrl.Result{}) {
		t.Errorf("pipeline: %+v %v", res, err)
	}
	req.Name = "r1"
	if res, err := NewRunReconciler(c, Config{}).Reconcile(context.Background(), req); err != nil || res != (ctrl.Result{}) {
		t.Errorf("run: %+v %v", res, err)
	}
}

func readyPipeline() *idpv1alpha1.DeliveryPipeline {
	dp := pipelineObj(goodRepo)
	dp.Status = idpv1alpha1.DeliveryPipelineStatus{Phase: idpv1alpha1.DeliveryPhaseReady, ApplicationName: "demo-orders"}
	return dp
}

func TestRunSuccess(t *testing.T) {
	c := newClient(t, readyPipeline(), runObj("r1", goodSHA))
	g := gateWithApp()
	r := runRec(c, g)

	res, run := reconcileRun(t, r, c, "r1")
	if run.Status.Phase != idpv1alpha1.DeliveryRunRunning || run.Status.ExecutionID != "01EXECUTIONID" || res.RequeueAfter != pollInterval {
		t.Fatalf("status = %+v, %+v", run.Status, res)
	}
	if len(g.Triggers) != 1 || g.Triggers[0]["ref"] != goodSHA {
		t.Errorf("triggers = %v", g.Triggers)
	}
	// Still running: no second trigger.
	_, run = reconcileRun(t, r, c, "r1")
	if run.Status.Phase != idpv1alpha1.DeliveryRunRunning || len(g.Triggers) != 1 {
		t.Errorf("phase = %s, triggers = %d", run.Status.Phase, len(g.Triggers))
	}
	g.Statuses["01EXECUTIONID"] = "SUCCEEDED"
	res, run = reconcileRun(t, r, c, "r1")
	if run.Status.Phase != idpv1alpha1.DeliveryRunSucceeded || res.RequeueAfter != 0 {
		t.Fatalf("status = %+v, %+v", run.Status, res)
	}
	// Terminal: nothing more happens, not even a Gate call.
	g.Calls = nil
	reconcileRun(t, r, c, "r1")
	if len(g.Calls) != 0 {
		t.Errorf("Gate called for a finished run: %v", g.Calls)
	}
}

func TestRunFailedExecution(t *testing.T) {
	for _, st := range []string{"TERMINAL", "CANCELED", "STOPPED"} {
		c := newClient(t, readyPipeline(), runObj("r1", goodSHA))
		g := gateWithApp()
		r := runRec(c, g)
		reconcileRun(t, r, c, "r1")
		g.Statuses["01EXECUTIONID"] = st
		_, run := reconcileRun(t, r, c, "r1")
		if run.Status.Phase != idpv1alpha1.DeliveryRunFailed || !strings.Contains(run.Status.Message, st) {
			t.Errorf("%s: %+v", st, run.Status)
		}
	}
}

func TestRunBadSHA(t *testing.T) {
	for _, ref := range []string{"main", goodSHA[:39], strings.ToUpper(goodSHA), "${bad}"} {
		c := newClient(t, readyPipeline(), runObj("r1", ref))
		g := newFakeGate()
		_, run := reconcileRun(t, runRec(c, g), c, "r1")
		if run.Status.Phase != idpv1alpha1.DeliveryRunFailed || len(g.Calls) != 0 {
			t.Errorf("ref %q: %+v calls %v", ref, run.Status, g.Calls)
		}
	}
}

func TestRunGateDown(t *testing.T) {
	c := newClient(t, readyPipeline(), runObj("r1", goodSHA))
	g := gateWithApp()
	g.Err = secretErr
	r := runRec(c, g)
	res, run := reconcileRun(t, r, c, "r1")
	if run.Status.Phase != idpv1alpha1.DeliveryRunPending || res.RequeueAfter != errorRetry || run.Status.ExecutionID != "" ||
		!strings.Contains(run.Status.Message, "HTTP 503") {
		t.Fatalf("status = %+v, %+v", run.Status, res)
	}
	// Recovers.
	g.Err = nil
	_, run = reconcileRun(t, r, c, "r1")
	if run.Status.Phase != idpv1alpha1.DeliveryRunRunning {
		t.Errorf("phase = %s", run.Status.Phase)
	}
	// Gate down while polling keeps the run RUNNING.
	g.Err = &GateError{Reason: "unreachable"}
	_, run = reconcileRun(t, r, c, "r1")
	if run.Status.Phase != idpv1alpha1.DeliveryRunRunning {
		t.Errorf("phase = %s", run.Status.Phase)
	}
	// ... until it times out.
	r.Now = func() time.Time { return time.Now().Add(RunTimeout + time.Minute) }
	_, run = reconcileRun(t, r, c, "r1")
	if run.Status.Phase != idpv1alpha1.DeliveryRunFailed {
		t.Errorf("phase = %s", run.Status.Phase)
	}
}

func TestRunGateRejectsTrigger(t *testing.T) {
	c := newClient(t, readyPipeline(), runObj("r1", goodSHA))
	g := gateWithApp()
	g.TriggerErr = &GateError{Status: 404, Reason: "Not Found"}
	_, run := reconcileRun(t, runRec(c, g), c, "r1")
	if run.Status.Phase != idpv1alpha1.DeliveryRunFailed || strings.Contains(run.Status.Message, "SECRET") {
		t.Errorf("status = %+v", run.Status)
	}
}

func TestRunPipelineStates(t *testing.T) {
	// Missing pipeline.
	c := newClient(t, runObj("r1", goodSHA))
	g := newFakeGate()
	_, run := reconcileRun(t, runRec(c, g), c, "r1")
	if run.Status.Phase != idpv1alpha1.DeliveryRunFailed || !strings.Contains(run.Status.Message, "does not exist") {
		t.Errorf("missing: %+v", run.Status)
	}
	// Building: the run waits.
	building := pipelineObj(goodRepo)
	building.Status.Phase = idpv1alpha1.DeliveryPhaseBuilding
	c = newClient(t, building, runObj("r1", goodSHA))
	res, run := reconcileRun(t, runRec(c, g), c, "r1")
	if run.Status.Phase != idpv1alpha1.DeliveryRunPending || res.RequeueAfter == 0 || len(g.Calls) != 0 {
		t.Errorf("building: %+v %+v %v", run.Status, res, g.Calls)
	}
	// Failed pipeline fails the run.
	failed := pipelineObj(goodRepo)
	failed.Status = idpv1alpha1.DeliveryPipelineStatus{Phase: idpv1alpha1.DeliveryPhaseFailed, Message: "boom"}
	c = newClient(t, failed, runObj("r1", goodSHA))
	_, run = reconcileRun(t, runRec(c, g), c, "r1")
	if run.Status.Phase != idpv1alpha1.DeliveryRunFailed || !strings.Contains(run.Status.Message, "boom") || len(g.Calls) != 0 {
		t.Errorf("failed: %+v", run.Status)
	}
	// A pipeline whose repo owner is no longer allowed is refused even if READY.
	rp := readyPipeline()
	rp.Spec.Repo = "https://github.com/evil/app"
	c = newClient(t, rp, runObj("r1", goodSHA))
	_, run = reconcileRun(t, runRec(c, g), c, "r1")
	if run.Status.Phase != idpv1alpha1.DeliveryRunFailed || len(g.Calls) != 0 {
		t.Errorf("disallowed: %+v calls %v", run.Status, g.Calls)
	}
}

func TestSafeStatusAndTruncation(t *testing.T) {
	if safeStatus("<script>") != "UNKNOWN" || safeStatus("TERMINAL") != "TERMINAL" {
		t.Error("safeStatus")
	}
	if len(trunc(strings.Repeat("x", 1000))) != maxMessage {
		t.Error("trunc")
	}
	if Safe(errors.New("x")) != "gate: unexpected error" {
		t.Error("Safe")
	}
}

func collisionPipeline(ns, wl string) *idpv1alpha1.DeliveryPipeline {
	return &idpv1alpha1.DeliveryPipeline{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: wl, Generation: 1},
		Spec:       idpv1alpha1.DeliveryPipelineSpec{Workload: wl, Repo: goodRepo},
	}
}

func reconcileOne(t *testing.T, r *PipelineReconciler, ns, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
		t.Fatal(err)
	}
}

// gateWithApp is a Gate whose application carries the marker of namespace demo, workload orders.
func gateWithApp() *fakeGate {
	g := newFakeGate()
	g.Apps["demo-orders"] = AppDescription("demo", "orders")
	return g
}

func TestRunRefusesForeignApplication(t *testing.T) {
	for name, desc := range map[string]string{"other pair": AppDescription("demo-orders", "x"), "unmarked": ""} {
		c := newClient(t, readyPipeline(), runObj("r1", goodSHA))
		g := newFakeGate()
		g.Apps["demo-orders"] = desc
		_, run := reconcileRun(t, runRec(c, g), c, "r1")
		if run.Status.Phase != idpv1alpha1.DeliveryRunFailed || len(g.Triggers) != 0 {
			t.Errorf("%s: %+v triggers=%d", name, run.Status, len(g.Triggers))
		}
	}
	c := newClient(t, readyPipeline(), runObj("r1", goodSHA))
	g := newFakeGate() // no application at all
	if _, run := reconcileRun(t, runRec(c, g), c, "r1"); run.Status.Phase != idpv1alpha1.DeliveryRunFailed || len(g.Triggers) != 0 {
		t.Errorf("missing application: %+v", run.Status)
	}
}

// A crash after RUNNING was written and before the execution id was: the run fails, no re-trigger.
func TestRunCrashAfterRunningIsNotRetriggered(t *testing.T) {
	run := runObj("r1", goodSHA)
	run.Status = idpv1alpha1.DeliveryRunStatus{Phase: idpv1alpha1.DeliveryRunRunning, Message: "starting the pipeline execution"}
	c := newClient(t, readyPipeline(), run)
	g := gateWithApp()
	_, got := reconcileRun(t, runRec(c, g), c, "r1")
	if got.Status.Phase != idpv1alpha1.DeliveryRunFailed || !strings.Contains(got.Status.Message, "interrupted") || len(g.Triggers) != 0 {
		t.Errorf("status = %+v triggers=%d", got.Status, len(g.Triggers))
	}
}

// RUNNING is written before the trigger, and a Gate error on the trigger fails the run instead of
// leaving it to be retried.
func TestRunTriggerErrorFailsAfterRunningWritten(t *testing.T) {
	c := newClient(t, readyPipeline(), runObj("r1", goodSHA))
	g := gateWithApp()
	g.TriggerErr = secretErr // a 503 may have started the execution: not retried
	r := runRec(c, g)
	_, got := reconcileRun(t, r, c, "r1")
	if got.Status.Phase != idpv1alpha1.DeliveryRunFailed || len(g.Triggers) != 0 {
		t.Fatalf("status = %+v", got.Status)
	}
	reconcileRun(t, r, c, "r1")
	if n := countCalls(g, "trigger:"); n != 1 {
		t.Errorf("triggered %d times", n)
	}
}

func countCalls(g *fakeGate, prefix string) int {
	n := 0
	for _, c := range g.Calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// A reconcile working from a stale cached copy (still PENDING after another one wrote RUNNING)
// conflicts on the RUNNING write and triggers nothing.
func TestRunStaleCacheDoesNotRetrigger(t *testing.T) {
	c := newClient(t, readyPipeline(), runObj("r1", goodSHA))
	g := gateWithApp()
	key := types.NamespacedName{Namespace: "demo", Name: "r1"}
	stale := &idpv1alpha1.DeliveryRun{}
	if err := c.Get(context.Background(), key, stale); err != nil {
		t.Fatal(err)
	}
	r := runRec(c, g)
	reconcileRun(t, r, c, "r1") // triggers once
	if len(g.Triggers) != 1 {
		t.Fatal("first reconcile did not trigger")
	}
	staleClient := interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, k client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if run, ok := obj.(*idpv1alpha1.DeliveryRun); ok && k == key {
				stale.DeepCopyInto(run)
				return nil
			}
			return cl.Get(ctx, k, obj, opts...)
		},
	})
	r2 := runRec(staleClient, g)
	if _, err := r2.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err == nil {
		t.Error("stale write did not conflict")
	}
	if len(g.Triggers) != 1 {
		t.Errorf("triggered %d times", len(g.Triggers))
	}
}

func TestRunExecutionOfAnotherApplicationFails(t *testing.T) {
	run := runObj("r1", goodSHA)
	run.Status = idpv1alpha1.DeliveryRunStatus{Phase: idpv1alpha1.DeliveryRunRunning, ExecutionID: "01OTHER"}
	c := newClient(t, readyPipeline(), run)
	g := gateWithApp()
	g.ExecApp = "someone-else"
	g.Statuses["01OTHER"] = "SUCCEEDED"
	_, got := reconcileRun(t, runRec(c, g), c, "r1")
	if got.Status.Phase != idpv1alpha1.DeliveryRunFailed || !strings.Contains(got.Status.Message, "does not belong") {
		t.Errorf("status = %+v", got.Status)
	}
}
