package pipelines

import (
	"context"
	"os"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/yaml"
)

func crdProps(t *testing.T, file string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../../operator/config/crd/bases/" + file)
	if os.IsNotExist(err) {
		t.Skipf("CRD %s not present", file)
	}
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	v := crd["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)
	spec, _, _ := unstructured.NestedMap(v, "schema", "openAPIV3Schema", "properties", "spec", "properties")
	return spec
}

func pattern(t *testing.T, props map[string]any, field string) string {
	t.Helper()
	p, _, _ := unstructured.NestedString(props, field, "pattern")
	if p == "" {
		t.Fatalf("no pattern for %s", field)
	}
	return p
}

// TestRulesMatchTheCRDs fails when the API's patterns drift from the CRDs', which would let the API
// accept a request the cluster rejects (or refuse one it accepts).
func TestRulesMatchTheCRDs(t *testing.T) {
	pp := crdProps(t, "idp.ithaca-idp.com_deliverypipelines.yaml")
	if got := pattern(t, pp, "repo"); got != strings.NewReplacer("(", "", ")", "").Replace(repoRe.String()) {
		t.Errorf("repo: CRD %q, API %q", got, repoRe)
	}
	if got := pattern(t, pp, "path"); got != pathRe.String() {
		t.Errorf("path: CRD %q, API %q", got, pathRe)
	}
	if got := pattern(t, pp, "workload"); got != labelRe.String() {
		t.Errorf("workload: CRD %q, API %q", got, labelRe)
	}
	rp := crdProps(t, "idp.ithaca-idp.com_deliveryruns.yaml")
	if got := pattern(t, rp, "ref"); got != shaRe.String() {
		t.Errorf("ref: CRD %q, API %q", got, shaRe)
	}
	if got := pattern(t, rp, "pipeline"); got != labelRe.String() {
		t.Errorf("pipeline: CRD %q, API %q", got, labelRe)
	}
	if n, _, _ := unstructured.NestedFloat64(pp, "workload", "maxLength"); int(n) != maxWorkloadLen {
		t.Errorf("workload maxLength: CRD %v, API %d", n, maxWorkloadLen)
	}
	if d, _, _ := unstructured.NestedString(pp, "path", "default"); d != DefaultPath {
		t.Errorf("default path: CRD %q, API %q", d, DefaultPath)
	}
}

func TestRunNameIsADNSLabel(t *testing.T) {
	for _, p := range []string{"a", strings.Repeat("a", 40)} {
		n, err := RunName(p)
		if err != nil || len(n) > 63 || !labelRe.MatchString(n) || !strings.HasPrefix(n, p+"-") || len(n) != len(p)+9 {
			t.Errorf("%q %v", n, err)
		}
	}
}

func fakeCluster() DynamicCluster {
	s := runtime.NewScheme()
	return DynamicCluster{Client: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(s, map[schema.GroupVersionResource]string{
		pipelineGVR: "DeliveryPipelineList", runGVR: "DeliveryRunList", workloadIdentityGVR: "WorkloadIdentityList"})}
}

func TestDynamicClusterWritesOnlyTheSpec(t *testing.T) {
	ctx := context.Background()
	c := fakeCluster()
	p := &Pipeline{Namespace: "team-a", Name: "orders", Workload: "orders", Repo: "https://github.com/ithaca-lab-org/orders", Path: "deploy/base"}
	if err := c.CreatePipeline(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := c.CreatePipeline(ctx, p); err != ErrExists {
		t.Fatalf("duplicate: %v", err)
	}
	obj, _ := c.Client.Resource(pipelineGVR).Namespace("team-a").Get(ctx, "orders", metav1.GetOptions{})
	md := obj.Object["metadata"].(map[string]any)
	for k := range md {
		if k != "name" && k != "namespace" && k != "creationTimestamp" {
			t.Errorf("unexpected metadata field %s", k)
		}
	}
	got, err := c.GetPipeline(ctx, "team-a", "orders")
	if err != nil || got.Phase != "PENDING" || got.Path != "deploy/base" || got.Workload != "orders" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := c.GetPipeline(ctx, "team-a", "nope"); err != ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
	// the controller reports; the portal reads it back
	_ = unstructured.SetNestedField(obj.Object, "READY", "status", "phase")
	_ = unstructured.SetNestedField(obj.Object, "team-a-orders", "status", "applicationName")
	if _, err := c.Client.Resource(pipelineGVR).Namespace("team-a").Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	l, err := c.ListPipelines(ctx, "team-a")
	if err != nil || len(l) != 1 || l[0].Phase != "READY" || l[0].ApplicationName != "team-a-orders" {
		t.Fatalf("%+v %v", l, err)
	}

	r := &Run{Namespace: "team-a", Name: "orders-0123abcd", Pipeline: "orders", Ref: strings.Repeat("a", 40), RequestedBy: "sub-1"}
	if err := c.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateRun(ctx, r); err != ErrExists {
		t.Fatalf("duplicate run: %v", err)
	}
	runs, err := c.ListRuns(ctx, "team-a")
	if err != nil || len(runs) != 1 || runs[0].RequestedBy != "sub-1" || runs[0].Phase != "PENDING" || runs[0].Pipeline != "orders" {
		t.Fatalf("%+v %v", runs, err)
	}
	if ok, err := c.WorkloadExists(ctx, "team-a", "orders"); err != nil || ok {
		t.Fatalf("workload: %v %v", ok, err)
	}
}
