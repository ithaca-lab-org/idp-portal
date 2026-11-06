package broker

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

func plainNS(t *testing.T, name string) {
	t.Helper()
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
		t.Fatal(err)
	}
}

func TestBucketClaimCRDValidation(t *testing.T) {
	requireEnvtest(t)
	ns := "crd-bc"
	plainNS(t, ns)
	dry := client.DryRunAll
	for _, n := range []string{strings.Repeat("a", 19), "Upper", "a.b", "-a", "a-", "a_b", "a b", strings.Repeat("a", 41)} {
		if err := k8s.Create(ctx, newClaim(ns, n), dry); err == nil {
			t.Errorf("name %q accepted", n)
		}
	}
	for _, n := range []string{"a", "data", "my-data-1", strings.Repeat("a", 18), "a--b"} {
		if err := k8s.Create(ctx, newClaim(ns, n), dry); err != nil {
			t.Errorf("name %q rejected: %v", n, err)
		}
	}
	// 4 + 31 + 1 + 18 + 1 + 8 is the longest bucket name there can be.
	if got, err := BucketName(testProjectNumber, strings.Repeat("n", 31), strings.Repeat("a", 18)); err != nil || len(got) != 63 {
		t.Errorf("longest bucket = %q (%v)", got, err)
	}

	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": idpv1alpha1.GroupVersion.String(), "kind": "BucketClaim",
		"metadata": map[string]any{"name": "extra", "namespace": ns},
		"spec":     map[string]any{"location": "europe-west1", "labels": map[string]any{"idp-managed": "false"}},
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

func TestResourceGrantCRDValidation(t *testing.T) {
	requireEnvtest(t)
	ns := "crd-rg"
	plainNS(t, ns)
	dry := client.DryRunAll
	if err := k8s.Create(ctx, newGrant(ns, "ok", "web", "data", "reader"), dry); err != nil {
		t.Fatalf("valid grant: %v", err)
	}
	if err := k8s.Create(ctx, newGrant(ns, "ok", strings.Repeat("w", 40), strings.Repeat("d", 18), "writer"), dry); err != nil {
		t.Fatalf("longest names: %v", err)
	}
	if err := k8s.Create(ctx, newGrant(ns, strings.Repeat("g", 63), "web", "data", "reader"), dry); err != nil {
		t.Errorf("63-char grant name: %v", err)
	}
	for _, w := range []string{"", "Web", "a.b", "-a", "a_b", strings.Repeat("w", 41), "vault-reader", "default",
		"default-token-x", "vault-reader-token-x", "sh-helm-x", "kube-x", "istio-x", "idp-x"} {
		if err := k8s.Create(ctx, newGrant(ns, "bad", w, "data", "reader"), dry); err == nil {
			t.Errorf("workload %q accepted", w)
		}
	}
	for _, r := range []string{"", "Data", "a.b", "-a", strings.Repeat("d", 19)} {
		if err := k8s.Create(ctx, newGrant(ns, "bad", "web", r, "reader"), dry); err == nil {
			t.Errorf("resource %q accepted", r)
		}
	}
	for _, a := range []string{"", "admin", "Reader", "owner", "roles/storage.admin", "reader,writer"} {
		if err := k8s.Create(ctx, newGrant(ns, "bad", "web", "data", a), dry); err == nil {
			t.Errorf("access %q accepted", a)
		}
	}
	// metadata.name stays a DNS-1123 label (the CRD is stricter than the apiserver's subdomain rule).
	for _, n := range []string{"a.b", strings.Repeat("g", 64)} {
		if err := k8s.Create(ctx, newGrant(ns, n, "web", "data", "reader"), dry); err == nil {
			t.Errorf("grant name %q accepted", n)
		}
	}
	// A missing field is refused.
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": idpv1alpha1.GroupVersion.String(), "kind": "ResourceGrant",
		"metadata": map[string]any{"name": "partial", "namespace": ns},
		"spec":     map[string]any{"workload": "web", "resource": "data"},
	}}
	if err := k8s.Create(ctx, u, dry); err == nil {
		t.Error("grant without access accepted")
	}
}

func TestResourceGrantSpecIsImmutable(t *testing.T) {
	requireEnvtest(t)
	ns := "crd-rg-imm"
	plainNS(t, ns)
	if err := k8s.Create(ctx, newGrant(ns, "g", "web", "data", "reader")); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*idpv1alpha1.ResourceGrant){
		"workload": func(g *idpv1alpha1.ResourceGrant) { g.Spec.Workload = "api" },
		"resource": func(g *idpv1alpha1.ResourceGrant) { g.Spec.Resource = "logs" },
		"access":   func(g *idpv1alpha1.ResourceGrant) { g.Spec.Access = "writer" },
	} {
		g := get(t, &idpv1alpha1.ResourceGrant{}, ns, "g")
		mutate(g)
		err := k8s.Update(ctx, g, client.DryRunAll)
		if err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Errorf("%s change: %v", name, err)
		}
	}
	// Metadata still changes (finalizers are how the broker works).
	g := get(t, &idpv1alpha1.ResourceGrant{}, ns, "g")
	g.Finalizers = []string{GrantFinalizerName}
	if err := k8s.Update(ctx, g, client.DryRunAll); err != nil {
		t.Errorf("finalizer update: %v", err)
	}
}
