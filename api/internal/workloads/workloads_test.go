package workloads

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"regexp"
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

func crdPath() string {
	if p := os.Getenv("WORKLOADIDENTITY_CRD"); p != "" {
		return p
	}
	return "../../../operator/config/crd/bases/idp.ithaca-idp.com_workloadidentities.yaml"
}

var quoted = regexp.MustCompile(`'([^']*)'`)

// TestNameRulesMatchTheCRD fails when the API's name rules drift from the CRD's
// x-kubernetes-validations, which would let the API accept a name the cluster rejects.
func TestNameRulesMatchTheCRD(t *testing.T) {
	raw, err := os.ReadFile(crdPath())
	if os.IsNotExist(err) {
		t.Skipf("WorkloadIdentity CRD not present at %s", crdPath())
	}
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Validations []struct {
							Rule string `json:"rule"`
						} `json:"x-kubernetes-validations"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions) == 0 {
		t.Fatal("CRD has no versions")
	}
	var names, prefixes []string
	sawLen, sawDNS := false, false
	for _, v := range crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Validations {
		r := v.Rule
		switch {
		case strings.Contains(r, "size() <= "):
			sawLen = strings.Contains(r, "size() <= 40")
		case strings.Contains(r, ".matches("):
			sawDNS = strings.Contains(r, `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
		case strings.Contains(r, "startsWith"):
			in, ex, ok := strings.Cut(r, "&&")
			if !ok {
				t.Fatalf("unexpected reserved-name rule: %s", r)
			}
			for _, m := range quoted.FindAllStringSubmatch(in, -1) {
				names = append(names, m[1])
			}
			for _, m := range quoted.FindAllStringSubmatch(ex, -1) {
				prefixes = append(prefixes, m[1])
			}
		}
	}
	if !sawLen || !sawDNS || secrets.MaxNameLen != 40 {
		t.Errorf("length or DNS-label rule differs from the API (len %v, dns %v)", sawLen, sawDNS)
	}
	if !reflect.DeepEqual(names, ReservedNames) || !reflect.DeepEqual(prefixes, ReservedPrefixes) {
		t.Errorf("reserved: CRD %q %q, API %q %q", names, prefixes, ReservedNames, ReservedPrefixes)
	}
}

func fakeDynamic(objs ...runtime.Object) DynamicCluster {
	s := runtime.NewScheme()
	return DynamicCluster{Client: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(s,
		map[schema.GroupVersionResource]string{workloadIdentityGVR: "WorkloadIdentityList"}, objs...)}
}

func TestDynamicClusterCreateGetList(t *testing.T) {
	ctx := context.Background()
	c := fakeDynamic()
	if err := c.Create(ctx, "team-a", "orders"); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, "team-a", "orders"); err != ErrExists {
		t.Fatalf("duplicate: %v", err)
	}
	w, err := c.Get(ctx, "team-a", "orders")
	if err != nil || w.Status != StatusPending || w.SpiffeID != "" || w.ServiceAccount != "orders" {
		t.Fatalf("%+v %v", w, err)
	}
	if _, err := c.Get(ctx, "team-a", "nope"); err != ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
	// the operator reports status; the portal reads it back unchanged
	obj, _ := c.Client.Resource(workloadIdentityGVR).Namespace("team-a").Get(ctx, "orders", metav1.GetOptions{})
	_ = unstructured.SetNestedField(obj.Object, "spiffe://idp.example/ns/team-a/sa/orders", "status", "spiffeID")
	_ = unstructured.SetNestedField(obj.Object, "orders.team-a.apps.example", "status", "hostname")
	_ = unstructured.SetNestedSlice(obj.Object, []any{map[string]any{"type": "Synced", "status": "True", "reason": "Synced", "message": "ok"}}, "status", "conditions")
	if _, err := c.Client.Resource(workloadIdentityGVR).Namespace("team-a").Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	l, err := c.List(ctx, "team-a")
	if err != nil || len(l) != 1 || l[0].Status != StatusComplete || l[0].SpiffeID != "spiffe://idp.example/ns/team-a/sa/orders" ||
		l[0].Hostname != "orders.team-a.apps.example" || len(l[0].Conditions) != 1 {
		t.Fatalf("%+v %v", l, err)
	}
}

func TestStatusOf(t *testing.T) {
	for _, tc := range []struct {
		c      []Condition
		status string
		reason string
	}{
		{nil, StatusPending, ""},
		{[]Condition{{Type: "Synced", Status: "True", Reason: "Synced"}}, StatusComplete, ""},
		{[]Condition{{Type: "Synced", Status: "False", Reason: "NameConflict"}}, StatusFailed, "NameConflict"},
		{[]Condition{{Type: "Synced", Status: "False", Reason: "PreviousTerminating"}}, StatusPending, "PreviousTerminating"},
		{[]Condition{{Type: "Other", Status: "False"}}, StatusPending, ""},
	} {
		if s, r := statusOf(tc.c); s != tc.status || r != tc.reason {
			t.Errorf("%+v: %s %s", tc.c, s, r)
		}
	}
}

func TestSyncErrorMessageIsGeneric(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "a", "namespace": "n"}}}
	_ = unstructured.SetNestedSlice(obj.Object, []any{
		map[string]any{"type": "Synced", "status": "False", "reason": "SyncError", "message": "vault at 10.0.0.5: permission denied"},
		map[string]any{"type": "Other", "status": "False", "reason": "NameConflict", "message": "ServiceAccount a exists"},
	}, "status", "conditions")
	w := fromObject(obj)
	if strings.Contains(w.Conditions[0].Message, "vault") || w.Conditions[0].Reason != "SyncError" || w.Conditions[0].Message == "" {
		t.Errorf("%+v", w.Conditions[0])
	}
	if w.Conditions[1].Message != "ServiceAccount a exists" {
		t.Errorf("other messages are kept: %+v", w.Conditions[1])
	}
}

func TestListFollowsContinueAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	mk := func(n int, cont string) *unstructured.UnstructuredList {
		l := &unstructured.UnstructuredList{}
		for i := 0; i < n; i++ {
			l.Items = append(l.Items, unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": fmt.Sprintf("w%d-%s", i, cont), "namespace": "n"}}})
		}
		l.SetContinue(cont)
		return l
	}
	c := fakeDynamic()
	calls := 0
	c.Client.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "workloadidentities", func(k clienttesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			return true, mk(3, "next"), nil
		}
		return true, mk(2, ""), nil
	})
	l, err := c.List(ctx, "n")
	if err != nil || len(l) != 5 || calls != 2 {
		t.Fatalf("%d %v calls %d", len(l), err, calls)
	}
	// endless pages: fail closed rather than return a partial list
	c2 := fakeDynamic()
	c2.Client.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "workloadidentities", func(k clienttesting.Action) (bool, runtime.Object, error) {
		return true, mk(100, "more"), nil
	})
	if _, err := c2.List(ctx, "n"); err == nil {
		t.Fatal("an over-large list must fail")
	}
}
