package delivery

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

// These tests need the envtest control plane (make test downloads it); they skip without it.

func makeNS(t *testing.T, name string, managed bool) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if managed {
		ns.Labels = map[string]string{"app.kubernetes.io/managed-by": "idp-operator", "idp.ithaca-idp.com/team-namespace": name}
	}
	if err := k8s.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}

func newDP(ns, name, workload, repo, path string) *idpv1alpha1.DeliveryPipeline {
	return &idpv1alpha1.DeliveryPipeline{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       idpv1alpha1.DeliveryPipelineSpec{Workload: workload, Repo: repo, Path: path},
	}
}

func newRun(ns, name, pipeline, ref string) *idpv1alpha1.DeliveryRun {
	return &idpv1alpha1.DeliveryRun{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       idpv1alpha1.DeliveryRunSpec{Pipeline: pipeline, Ref: ref, RequestedBy: "sub-1"},
	}
}

func TestDeliveryPipelineCRDValidation(t *testing.T) {
	requireEnvtest(t)
	makeNS(t, "crd-dp", false)
	dry := client.DryRunAll
	good := "https://github.com/ithaca-lab-org/sample-orders-api"
	if err := k8s.Create(ctx, newDP("crd-dp", "orders", "orders", good, ""), dry); err != nil {
		t.Fatalf("valid pipeline: %v", err)
	}
	dp := newDP("crd-dp", "orders", "orders", good, "")
	if err := k8s.Create(ctx, dp, dry); err != nil || dp.Spec.Path != "deploy/base" {
		t.Errorf("default path = %q (%v)", dp.Spec.Path, err)
	}
	if err := k8s.Create(ctx, newDP("crd-dp", "orders", "orders", good, "k8s/prod.d"), dry); err != nil {
		t.Errorf("nested path: %v", err)
	}
	for name, dp := range map[string]*idpv1alpha1.DeliveryPipeline{
		"name differs from workload": newDP("crd-dp", "web", "orders", good, ""),
		"upper workload":             newDP("crd-dp", "Orders", "Orders", good, ""),
		"empty workload":             newDP("crd-dp", "orders", "", good, ""),
		"long workload":              newDP("crd-dp", strings.Repeat("w", 41), strings.Repeat("w", 41), good, ""),
		"http":                       newDP("crd-dp", "orders", "orders", "http://github.com/o/r", ""),
		"other host":                 newDP("crd-dp", "orders", "orders", "https://gitlab.com/o/r", ""),
		"userinfo":                   newDP("crd-dp", "orders", "orders", "https://u@github.com/o/r", ""),
		"deep url":                   newDP("crd-dp", "orders", "orders", good+"/tree/main", ""),
		"trailing slash":             newDP("crd-dp", "orders", "orders", good+"/", ""),
		"absolute path":              newDP("crd-dp", "orders", "orders", good, "/etc"),
		"dotdot":                     newDP("crd-dp", "orders", "orders", good, "a/../b"),
		"leading dotdot":             newDP("crd-dp", "orders", "orders", good, ".."),
		"double slash":               newDP("crd-dp", "orders", "orders", good, "a//b"),
		"trailing slash path":        newDP("crd-dp", "orders", "orders", good, "a/"),
		"space in path":              newDP("crd-dp", "orders", "orders", good, "a b"),
	} {
		if err := k8s.Create(ctx, dp, dry); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	// The workload is immutable.
	if err := k8s.Create(ctx, newDP("crd-dp", "imm", "imm", good, "")); err != nil {
		t.Fatal(err)
	}
	cur := &idpv1alpha1.DeliveryPipeline{}
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: "crd-dp", Name: "imm"}, cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Workload = "other"
	if err := k8s.Update(ctx, cur); err == nil {
		t.Error("workload changed")
	}
	cur.Spec.Workload = "imm"
	cur.Spec.Path = "x/y"
	if err := k8s.Update(ctx, cur); err != nil {
		t.Errorf("path change: %v", err)
	}
	// Status phases are an enum.
	cur.Status.Phase = "DONE"
	if err := k8s.Status().Update(ctx, cur); err == nil {
		t.Error("bad phase accepted")
	}
}

func TestDeliveryRunCRDValidation(t *testing.T) {
	requireEnvtest(t)
	makeNS(t, "crd-dr", false)
	dry := client.DryRunAll
	sha := strings.Repeat("ab", 20)
	if err := k8s.Create(ctx, newRun("crd-dr", "r", "orders", sha), dry); err != nil {
		t.Fatalf("valid run: %v", err)
	}
	for name, r := range map[string]*idpv1alpha1.DeliveryRun{
		"branch":     newRun("crd-dr", "r", "orders", "main"),
		"short sha":  newRun("crd-dr", "r", "orders", sha[:39]),
		"upper sha":  newRun("crd-dr", "r", "orders", strings.ToUpper(sha)),
		"sha plus":   newRun("crd-dr", "r", "orders", sha+"0"),
		"newline":    newRun("crd-dr", "r", "orders", sha[:39]+"\n"),
		"spel":       newRun("crd-dr", "r", "orders", "${1+1}"),
		"empty pipe": newRun("crd-dr", "r", "", sha),
		"upper pipe": newRun("crd-dr", "r", "Orders", sha),
		"long subject": func() *idpv1alpha1.DeliveryRun {
			r := newRun("crd-dr", "r", "orders", sha)
			r.Spec.RequestedBy = strings.Repeat("s", 129)
			return r
		}(),
	} {
		if err := k8s.Create(ctx, r, dry); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := k8s.Create(ctx, newRun("crd-dr", "imm", "orders", sha)); err != nil {
		t.Fatal(err)
	}
	cur := &idpv1alpha1.DeliveryRun{}
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: "crd-dr", Name: "imm"}, cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Ref = strings.Repeat("cd", 20)
	if err := k8s.Update(ctx, cur); err == nil {
		t.Error("run spec changed")
	}
}

// --- admission policies -----------------------------------------------------------------------

const (
	portalUser   = "system:serviceaccount:idp-system:portal-api"
	deliveryUser = "system:serviceaccount:idp-system:idp-delivery"
	tenantUser   = "tenant@example.com"
)

var (
	polOnce sync.Once
	polErr  error
)

func asUser(user string, groups ...string) client.Client {
	c := rest.CopyConfig(cfg)
	c.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: groups}
	cl, err := client.New(c, client.Options{Scheme: testScheme})
	if err != nil {
		panic(err)
	}
	return cl
}

func applyFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(&u.Object); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if len(u.Object) == 0 {
			continue
		}
		if err := k8s.Create(ctx, u); err != nil {
			return err
		}
	}
}

func isDenied(err error, policy string) bool {
	return apierrors.IsForbidden(err) && strings.Contains(err.Error(), policy)
}

// policyClients installs the policies and the roles the two ServiceAccounts and a tenant would
// have, and waits until the policies are enforced.
func policyClients(t *testing.T) (portal, delivery, tenant client.Client) {
	t.Helper()
	requireEnvtest(t)
	polOnce.Do(func() {
		if polErr = k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "idp-system"}}); polErr != nil {
			return
		}
		for _, f := range []string{"delivery-scope.yaml", "portal-scope.yaml"} {
			if polErr = applyFile(filepath.Join("..", "..", "config", "policy", f)); polErr != nil {
				return
			}
		}
		if polErr = applyFile(filepath.Join("..", "..", "config", "rbac", "delivery-role.yaml")); polErr != nil {
			return
		}
		// The portal's own role lives in idp-gitops; mirror what it needs. The tenant gets
		// everything on these kinds, to show the policy (not RBAC) stops it.
		u := func(yaml string) error {
			obj := &unstructured.Unstructured{}
			if err := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(yaml), 1024).Decode(&obj.Object); err != nil {
				return err
			}
			return k8s.Create(ctx, obj)
		}
		for _, y := range []string{
			`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"t-dlv"},"rules":[{"apiGroups":["idp.ithaca-idp.com"],"resources":["deliverypipelines","deliveryruns","deliverypipelines/status","deliveryruns/status"],"verbs":["*"]}]}`,
			`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRoleBinding","metadata":{"name":"t-dlv-portal"},"roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"ClusterRole","name":"t-dlv"},"subjects":[{"kind":"User","name":"` + portalUser + `"},{"kind":"User","name":"` + tenantUser + `"}]}`,
			`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRoleBinding","metadata":{"name":"idp-delivery"},"roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"ClusterRole","name":"idp-delivery"},"subjects":[{"kind":"User","name":"` + deliveryUser + `"}]}`,
		} {
			if polErr = u(y); polErr != nil {
				return
			}
		}
	})
	if polErr != nil {
		t.Fatalf("policy setup: %v", polErr)
	}
	makeNS(t, "pol-dlv", true)
	portal = asUser(portalUser, "system:authenticated")
	delivery = asUser(deliveryUser, "system:authenticated")
	tenant = asUser(tenantUser, "system:authenticated")
	// Wait for enforcement: a tenant create must be refused by the policy.
	eventually(t, "delivery-writers policy active", func() error {
		err := tenant.Create(ctx, newRun("pol-dlv", "probe", "orders", strings.Repeat("ab", 20)), client.DryRunAll)
		if isDenied(err, "idp-delivery-writers") {
			return nil
		}
		return errors.New("not enforced yet")
	})
	return
}

func TestDeliveryPolicies(t *testing.T) {
	portal, delivery, tenant := policyClients(t)
	good := "https://github.com/ithaca-lab-org/sample-orders-api"
	sha := strings.Repeat("ab", 20)
	ns := "pol-dlv"

	// The portal creates a pipeline and a run in a managed namespace.
	if err := portal.Create(ctx, newDP(ns, "orders", "orders", good, "")); err != nil {
		t.Fatalf("portal create pipeline: %v", err)
	}
	if err := portal.Create(ctx, newRun(ns, "run1", "orders", sha)); err != nil {
		t.Fatalf("portal create run: %v", err)
	}
	// ... but not elsewhere, not with labels, not a second time as an update, not a delete.
	makeNS(t, "pol-plain", false)
	if err := portal.Create(ctx, newDP("pol-plain", "orders", "orders", good, ""), client.DryRunAll); !isDenied(err, "idp-portal-scope") {
		t.Errorf("unmanaged namespace: %v", err)
	}
	lab := newRun(ns, "run-lab", "orders", sha)
	lab.Labels = map[string]string{"a": "b"}
	if err := portal.Create(ctx, lab, client.DryRunAll); !isDenied(err, "idp-portal-scope") {
		t.Errorf("labels: %v", err)
	}
	cur := &idpv1alpha1.DeliveryPipeline{}
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "orders"}, cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Path = "x"
	if err := portal.Update(ctx, cur); !isDenied(err, "idp-portal-scope") {
		t.Errorf("portal update: %v", err)
	}
	if err := portal.Delete(ctx, cur); !isDenied(err, "idp-portal-scope") {
		t.Errorf("portal delete: %v", err)
	}
	cur.Status.Phase = "READY"
	if err := portal.Status().Update(ctx, cur); !isDenied(err, "idp-portal-scope") {
		t.Errorf("portal status: %v", err)
	}

	// A tenant with full RBAC on the kinds still cannot create, update or fake a status.
	if err := tenant.Create(ctx, newDP(ns, "web", "web", good, ""), client.DryRunAll); !isDenied(err, "idp-delivery-writers") {
		t.Errorf("tenant create pipeline: %v", err)
	}
	cur = &idpv1alpha1.DeliveryPipeline{}
	_ = k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "orders"}, cur)
	cur.Spec.Path = "x"
	if err := tenant.Update(ctx, cur, client.DryRunAll); !isDenied(err, "idp-delivery-writers") {
		t.Errorf("tenant update: %v", err)
	}
	cur.Status.Phase = "READY"
	if err := tenant.Status().Update(ctx, cur); !isDenied(err, "idp-delivery-writers") {
		t.Errorf("tenant status: %v", err)
	}

	// The delivery controller writes status, and only status.
	cur = &idpv1alpha1.DeliveryPipeline{}
	_ = k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "orders"}, cur)
	cur.Status = idpv1alpha1.DeliveryPipelineStatus{Phase: "READY", ApplicationName: ns + "-orders", Message: "ok"}
	if err := delivery.Status().Update(ctx, cur); err != nil {
		t.Fatalf("delivery status: %v", err)
	}
	cur.Status.ApplicationName = "other-app"
	if err := delivery.Status().Update(ctx, cur); !isDenied(err, "idp-delivery-scope") {
		t.Errorf("foreign applicationName: %v", err)
	}
	cur.Status.ApplicationName = ns + "-orders"
	cur.Spec.Path = "changed"
	if err := delivery.Update(ctx, cur); !apierrors.IsForbidden(err) { // RBAC refuses first; the policy is the second line
		t.Errorf("delivery main update: %v", err)
	}
	if err := delivery.Create(ctx, newRun(ns, "mine", "orders", sha), client.DryRunAll); !isDenied(err, "idp-delivery-scope") && !apierrors.IsForbidden(err) {
		t.Errorf("delivery create: %v", err)
	}
	if err := delivery.Delete(ctx, cur); err == nil {
		t.Error("delivery delete allowed")
	}
	run := &idpv1alpha1.DeliveryRun{}
	_ = k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "run1"}, run)
	run.Status = idpv1alpha1.DeliveryRunStatus{Phase: "RUNNING", ExecutionID: "01HEXEC", Message: "started"}
	if err := delivery.Status().Update(ctx, run); err != nil {
		t.Fatalf("delivery run status: %v", err)
	}
	run.Status.ExecutionID = "../../x"
	if err := delivery.Status().Update(ctx, run); err == nil {
		t.Error("odd execution id accepted")
	}
	// Nor the WorkloadIdentity: read only.
	wi := &idpv1alpha1.WorkloadIdentity{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "orders"}}
	if err := delivery.Create(ctx, wi, client.DryRunAll); !apierrors.IsForbidden(err) {
		t.Errorf("delivery created a workloadidentity: %v", err)
	}
	if err := delivery.List(ctx, &idpv1alpha1.WorkloadIdentityList{}, client.InNamespace(ns)); err != nil {
		t.Errorf("delivery cannot list workloadidentities: %v", err)
	}
	if err := delivery.Get(ctx, types.NamespacedName{Namespace: ns, Name: "x"}, &corev1.Secret{}); !apierrors.IsForbidden(err) {
		t.Errorf("delivery can read secrets: %v", err)
	}
}
