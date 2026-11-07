package resources

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/ithaca-lab-org/idp-portal/api/internal/secrets"
)

// The CRDs and the broker's rules are generated or written in the operator module, which this
// module cannot import; the tests read their files instead. Each path can be overridden to check
// another branch's copy, and a missing file skips the test.
func fromEnv(env, rel string) string {
	if p := os.Getenv(env); p != "" {
		return p
	}
	return "../../../operator/" + rel
}

var (
	claimCRDPath = func() string {
		return fromEnv("BUCKETCLAIM_CRD", "config/crd/bases/idp.ithaca-idp.com_bucketclaims.yaml")
	}
	grantCRDPath = func() string {
		return fromEnv("RESOURCEGRANT_CRD", "config/crd/bases/idp.ithaca-idp.com_resourcegrants.yaml")
	}
	brokerRules = func() string { return fromEnv("BROKER_RULES", "internal/broker/rules.go") }
)

func readOrSkip(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("%s not present", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type crdSchema struct {
	Spec struct {
		Versions []struct {
			Schema struct {
				OpenAPIV3Schema struct {
					Validations []struct {
						Rule string `json:"rule"`
					} `json:"x-kubernetes-validations"`
					Properties struct {
						Spec struct {
							Properties map[string]struct {
								Enum      []string `json:"enum"`
								MaxLength int      `json:"maxLength"`
							} `json:"properties"`
						} `json:"spec"`
					} `json:"properties"`
				} `json:"openAPIV3Schema"`
			} `json:"schema"`
		} `json:"versions"`
	} `json:"spec"`
}

func parseCRD(t *testing.T, path string) crdSchema {
	t.Helper()
	var crd crdSchema
	if err := yaml.Unmarshal(readOrSkip(t, path), &crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions) == 0 {
		t.Fatal("CRD has no versions")
	}
	return crd
}

// TestRulesMatchTheCRDAndTheBroker fails when the API's rules drift from the BucketClaim and
// ResourceGrant CRDs (x-kubernetes-validations, enum, maxLength) or from the broker's constants
// in operator/internal/broker/rules.go (read as text), which would let the API accept what the
// cluster or the broker rejects.
func TestRulesMatchTheCRDAndTheBroker(t *testing.T) {
	claim := parseCRD(t, claimCRDPath()).Spec.Versions[0].Schema.OpenAPIV3Schema
	sawLen, sawDNS := false, false
	for _, v := range claim.Validations {
		switch {
		case strings.Contains(v.Rule, "size() <= "):
			sawLen = strings.Contains(v.Rule, "size() <= "+strconv.Itoa(MaxNameLen))
		case strings.Contains(v.Rule, ".matches("):
			sawDNS = strings.Contains(v.Rule, dnsLabel.String())
		}
	}
	if !sawLen || !sawDNS {
		t.Errorf("BucketClaim name rule differs from the API (len %v, dns %v)", sawLen, sawDNS)
	}

	grant := parseCRD(t, grantCRDPath()).Spec.Versions[0].Schema.OpenAPIV3Schema
	props := grant.Properties.Spec.Properties
	var menu []string
	for _, a := range AccessMenu {
		menu = append(menu, a.Value)
	}
	if !reflect.DeepEqual(props["access"].Enum, menu) {
		t.Errorf("access enum: CRD %v, API %v", props["access"].Enum, menu)
	}
	if props["resource"].MaxLength != MaxNameLen || props["workload"].MaxLength != secrets.MaxNameLen {
		t.Errorf("grant lengths: CRD resource %d workload %d, API %d and %d",
			props["resource"].MaxLength, props["workload"].MaxLength, MaxNameLen, secrets.MaxNameLen)
	}
	// a grant name is <workload>-<bucket>: it must fit the grant CRD's own name rule
	if MaxNameLen+1+secrets.MaxNameLen > 63 {
		t.Error("the longest grant name does not fit 63 characters")
	}
	saw63 := false
	for _, v := range grant.Validations {
		saw63 = saw63 || strings.Contains(v.Rule, "size() <= 63")
	}
	if !saw63 {
		t.Error("ResourceGrant name rule differs from the API")
	}

	// the broker: 4 + namespace + 1 + claim + 1 + 8 must be 63
	src := string(readOrSkip(t, brokerRules()))
	num := func(name string) int {
		m := regexp.MustCompile(name + `\s*=\s*(\d+)`).FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("%s not found in the broker rules", name)
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	if num("MaxNamespaceLen") != MaxNamespaceLen || num("MaxClaimLen") != MaxNameLen {
		t.Errorf("broker limits: namespace %d claim %d, API %d and %d", num("MaxNamespaceLen"), num("MaxClaimLen"), MaxNamespaceLen, MaxNameLen)
	}
	if num("MaxBucketLen") != len("idp-")+MaxNamespaceLen+1+MaxNameLen+1+num("hashLen") {
		t.Error("the API limits no longer add up to the broker's bucket name length")
	}
	for _, want := range []string{`strings.HasPrefix(p, "goog")`, `strings.Contains(p, "google")`, `BucketPrefix = "idp-"`} {
		if !strings.Contains(src, want) {
			t.Errorf("the broker's name rule changed (%s not found); update refused() too", want)
		}
	}
}

func TestValidateBucketName(t *testing.T) {
	for name, ok := range map[string]bool{
		"data": true, "a": true, "a-b-c": true, strings.Repeat("a", 18): true, "n1": true,
		"": false, strings.Repeat("a", 19): false, "Data": false, "a.b": false, "-a": false, "a-": false, "a_b": false,
		"google": false, "mygoogle": false, "goog": false, "goodbye": true, "googly": false, "agoog": true, "x-google-y": false,
	} {
		if err := ValidateBucketName(name); (err == nil) != ok {
			t.Errorf("%q: %v", name, err)
		}
	}
	for ns, ok := range map[string]bool{
		"team-a": true, strings.Repeat("a", 31): true, strings.Repeat("a", 32): false, strings.Repeat("a", 40): false,
		"google-team": false, "goog-team": false, "team-google": false, "good-team": true,
	} {
		if err := validateNamespace(ns); (err == nil) != ok {
			t.Errorf("namespace %q: %v", ns, err)
		}
	}
	if ValidateAccess("reader") != nil || ValidateAccess("writer") != nil || ValidateAccess("admin") == nil || ValidateAccess("") == nil {
		t.Error("access menu")
	}
	if GrantName("w", "b") != "w-b" || len(GrantName(strings.Repeat("a", 40), strings.Repeat("b", 18))) != 59 {
		t.Error("grant name")
	}
}

func fakeDynamic(objs ...runtime.Object) DynamicCluster {
	return DynamicCluster{Client: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			bucketClaimGVR: "BucketClaimList", resourceGrantGVR: "ResourceGrantList", workloadIdentityGVR: "WorkloadIdentityList",
		}, objs...)}
}

func setStatus(t *testing.T, c DynamicCluster, gvr schema.GroupVersionResource, ns, name string, status map[string]any) {
	t.Helper()
	ctx := context.Background()
	obj, err := c.Client.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	obj.Object["status"] = status
	if _, err := c.Client.Resource(gvr).Namespace(ns).Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func readyStatus(status, reason, msg string) map[string]any {
	return map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": status, "reason": reason, "message": msg}}}
}

func TestDynamicClusterBuckets(t *testing.T) {
	ctx := context.Background()
	c := fakeDynamic()
	if err := c.CreateBucket(ctx, "team-a", "data"); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateBucket(ctx, "team-a", "data"); err != ErrBucketExists {
		t.Fatalf("duplicate: %v", err)
	}
	// the claim is created with an empty spec and nothing else
	obj, _ := c.Client.Resource(bucketClaimGVR).Namespace("team-a").Get(ctx, "data", metav1.GetOptions{})
	if spec, _, _ := unstructured.NestedMap(obj.Object, "spec"); len(spec) != 0 || len(obj.GetLabels()) != 0 || len(obj.GetAnnotations()) != 0 || len(obj.GetFinalizers()) != 0 {
		t.Fatalf("unexpected content: %v", obj.Object)
	}
	b, err := c.GetBucket(ctx, "team-a", "data")
	if err != nil || b.Status != StatusPending || b.BucketName != "" {
		t.Fatalf("%+v %v", b, err)
	}
	if _, err := c.GetBucket(ctx, "team-a", "nope"); err != ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
	st := readyStatus("True", "Ready", "bucket idp-team-a-data-0123abcd exists")
	st["bucketName"] = "idp-team-a-data-0123abcd"
	setStatus(t, c, bucketClaimGVR, "team-a", "data", st)
	l, err := c.ListBuckets(ctx, "team-a")
	if err != nil || len(l) != 1 || l[0].Status != StatusComplete || l[0].BucketName != "idp-team-a-data-0123abcd" || l[0].Reason != "" || l[0].Message != "" {
		t.Fatalf("%+v %v", l, err)
	}
	// only the exact name for this claim is shown
	for _, odd := range []string{"https://evil.example/x", "idp-team-b-data-0123abcd", "idp-team-a-other-0123abcd", "idp-team-a-data-0123ABCD",
		"idp-team-a-data-0123abc", "idp-team-a-data-0123abcde", "idp-team-a-data-0123abcd/x", "idp-team-a-data-0123abcd\n"} {
		st["bucketName"] = odd
		setStatus(t, c, bucketClaimGVR, "team-a", "data", st)
		if b, _ := c.GetBucket(ctx, "team-a", "data"); b.BucketName != "" {
			t.Fatalf("odd bucket name relayed: %q", b.BucketName)
		}
	}
}

func TestBucketMessagesAreNeverRelayed(t *testing.T) {
	const leak = "googleapi: Error 403: idp-broker@proj-123.iam.gserviceaccount.com does not have storage.buckets.create access"
	for reason, want := range map[string]string{
		"CreateFailed": StatusFailed, "NameTaken": StatusFailed, "NamespaceNotManaged": StatusFailed, "InvalidName": StatusFailed,
		"Terminating": StatusFailed, "Orphaned": StatusFailed, "SomethingNew": StatusFailed,
	} {
		obj := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "a", "namespace": "n"}, "status": readyStatus("False", reason, leak)}}
		b := bucketFrom(obj)
		if b.Status != want || b.Message == "" || strings.Contains(b.Message, "google") || strings.Contains(b.Message, "iam") || strings.Contains(b.Message, "proj-123") {
			t.Errorf("%s: %+v", reason, b)
		}
		if reason == "SomethingNew" && b.Reason != "Unknown" {
			t.Errorf("an unknown reason is relayed: %q", b.Reason)
		}
		if reason == "CreateFailed" && (b.Message != generic || b.Reason != "CreateFailed") {
			t.Errorf("CreateFailed: %+v", b)
		}
	}
	for reason, want := range map[string]string{
		"GrantFailed": StatusFailed, "WorkloadNotFound": StatusFailed, "ClaimNotFound": StatusFailed, "ClaimNotReady": StatusPending,
		"NotManaged": StatusFailed, "Other": StatusFailed,
	} {
		obj := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "w-a", "namespace": "n"},
			"spec": map[string]any{"workload": "w", "resource": "a", "access": "reader"}, "status": readyStatus("False", reason, leak)}}
		g := grantFrom(obj)
		if g.Status != want || g.Message == "" || strings.Contains(g.Message, "google") || strings.Contains(g.Message, "proj-123") ||
			g.Workload != "w" || g.Bucket != "a" || g.Access != "reader" {
			t.Errorf("%s: %+v", reason, g)
		}
	}
}

func TestPhaseOfBucketsAndGrants(t *testing.T) {
	mk := func(status map[string]any) *unstructured.Unstructured {
		o := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "a", "namespace": "n"}}}
		if status != nil {
			o.Object["status"] = status
		}
		return o
	}
	for _, tc := range []struct {
		status map[string]any
		want   string
	}{
		{nil, StatusPending},
		{readyStatus("Unknown", "", ""), StatusPending},
		{readyStatus("True", "Ready", ""), StatusComplete},
		{readyStatus("False", "NameTaken", ""), StatusFailed},
		{map[string]any{"conditions": []any{map[string]any{"type": "Other", "status": "False"}}}, StatusPending},
	} {
		if got := bucketFrom(mk(tc.status)).Status; got != tc.want {
			t.Errorf("%v: %s", tc.status, got)
		}
	}
	// a grant being deleted is shown as being revoked, whatever its status says
	g := mk(readyStatus("True", "Bound", ""))
	now := metav1.Now()
	g.SetDeletionTimestamp(&now)
	if got := grantFrom(g); got.Status != StatusPending || got.Reason != "Revoking" {
		t.Errorf("%+v", got)
	}
	if got := grantFrom(mk(readyStatus("True", "Bound", ""))); got.Status != StatusComplete || got.Reason != "" || got.Message != "" {
		t.Errorf("%+v", got)
	}
}

func TestDynamicClusterGrants(t *testing.T) {
	ctx := context.Background()
	c := fakeDynamic()
	if err := c.CreateGrant(ctx, "team-a", "orders-data", "orders", "data", "writer"); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateGrant(ctx, "team-a", "orders-data", "orders", "data", "reader"); err != ErrGrantExists {
		t.Fatalf("duplicate: %v", err)
	}
	obj, _ := c.Client.Resource(resourceGrantGVR).Namespace("team-a").Get(ctx, "orders-data", metav1.GetOptions{})
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	if !reflect.DeepEqual(spec, map[string]any{"workload": "orders", "resource": "data", "access": "writer"}) || len(obj.GetLabels()) != 0 || len(obj.GetFinalizers()) != 0 {
		t.Fatalf("unexpected content: %v", obj.Object)
	}
	setStatus(t, c, resourceGrantGVR, "team-a", "orders-data", readyStatus("True", "Bound", "bound"))
	l, err := c.ListGrants(ctx, "team-a")
	if err != nil || len(l) != 1 || l[0].Status != StatusComplete || l[0].Workload != "orders" || l[0].Bucket != "data" || l[0].Access != "writer" {
		t.Fatalf("%+v %v", l, err)
	}
	// same pair again: a repeat; another pair with the same name: a collision
	if err := c.CreateGrant(ctx, "team-a", "orders-data", "orders", "data", "reader"); err != ErrGrantExists {
		t.Fatalf("repeat: %v", err)
	}
	if err := c.CreateGrant(ctx, "team-a", "orders-data", "orders-d", "ata", "reader"); err != ErrGrantCollision {
		t.Fatalf("collision: %v", err)
	}
	// the same name in another namespace is another grant, and deleting one leaves the other
	if err := c.CreateGrant(ctx, "team-b", "orders-data", "orders", "data", "reader"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteGrant(ctx, "team-b", "orders-data"); err != nil {
		t.Fatal(err)
	}
	if l, _ := c.ListGrants(ctx, "team-a"); len(l) != 1 {
		t.Fatalf("a delete in team-b reached team-a: %+v", l)
	}
	if err := c.DeleteGrant(ctx, "team-a", "nope"); err != ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
	if err := c.DeleteGrant(ctx, "team-a", "orders-data"); err != nil {
		t.Fatal(err)
	}
	if l, _ := c.ListGrants(ctx, "team-a"); len(l) != 0 {
		t.Fatalf("not deleted: %+v", l)
	}
	if ok, err := c.WorkloadExists(ctx, "team-a", "orders"); ok || err != nil {
		t.Fatalf("missing workload: %v %v", ok, err)
	}
	wi := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "idp.ithaca-idp.com/v1alpha1", "kind": "WorkloadIdentity",
		"metadata": map[string]any{"name": "orders", "namespace": "team-a"}}}
	if _, err := c.Client.Resource(workloadIdentityGVR).Namespace("team-a").Create(ctx, wi, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.WorkloadExists(ctx, "team-a", "orders"); !ok || err != nil {
		t.Fatalf("workload: %v %v", ok, err)
	}
}

func TestListFollowsContinueAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	mk := func(n int, cont string) *unstructured.UnstructuredList {
		l := &unstructured.UnstructuredList{}
		for i := 0; i < n; i++ {
			l.Items = append(l.Items, unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": fmt.Sprintf("b%d-%s", i, cont), "namespace": "n"}}})
		}
		l.SetContinue(cont)
		return l
	}
	c := fakeDynamic()
	calls := 0
	c.Client.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "bucketclaims", func(k clienttesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			return true, mk(3, "next"), nil
		}
		return true, mk(2, ""), nil
	})
	l, err := c.ListBuckets(ctx, "n")
	if err != nil || len(l) != 5 || calls != 2 {
		t.Fatalf("%d %v calls %d", len(l), err, calls)
	}
	// endless pages: fail closed rather than return a partial list (and a cap check on it)
	c2 := fakeDynamic()
	c2.Client.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "resourcegrants", func(k clienttesting.Action) (bool, runtime.Object, error) {
		return true, mk(100, "more"), nil
	})
	if _, err := c2.ListGrants(ctx, "n"); err == nil {
		t.Fatal("an over-large list must fail")
	}
}
