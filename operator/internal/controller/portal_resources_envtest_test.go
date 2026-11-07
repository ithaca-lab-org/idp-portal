package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

func newBC(ns, name string) *idpv1alpha1.BucketClaim {
	return &idpv1alpha1.BucketClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

// newRG is a grant named the way the portal names it: <workload>-<resource>.
func newRG(ns, workload, resource, access string) *idpv1alpha1.ResourceGrant {
	return &idpv1alpha1.ResourceGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: workload + "-" + resource},
		Spec:       idpv1alpha1.ResourceGrantSpec{Workload: workload, Resource: resource, Access: access},
	}
}

// TestPortalResourcesScope covers the portal's writes of BucketClaims and ResourceGrants (ADR 0010).
// The CRDs validate first (name length and format, the access enum, a grant's fields, immutable
// fields), so those cases are checked as plain refusals; the cases only this policy decides
// (managed namespace, no labels, annotations, finalizers or owners, the grant's name, the
// namespace length and "goog" rules, create-only claims, no updates) must name the policy.
func TestPortalResourcesScope(t *testing.T) {
	pc := portalAPIClient(t)
	managed := "team-res-portal"
	createAndReconcile(t, newTN(managed))
	plain := "plain-ns"
	dry := client.DryRunAll
	wantPortal := func(what string, err error) {
		t.Helper()
		if !isPortalDenial(err) {
			t.Errorf("%s: want denial by %s, got %v", what, portalPolicyName, err)
		}
	}

	// Allowed: an empty claim and a grant on it in a managed namespace; get, list; revoke the grant.
	if err := pc.Create(ctx, newBC(managed, "data")); err != nil {
		t.Fatalf("portal create bucketclaim in managed ns: %v", err)
	}
	if err := pc.Create(ctx, newRG(managed, "orders", "data", "reader")); err != nil {
		t.Fatalf("portal create resourcegrant in managed ns: %v", err)
	}
	if err := pc.Create(ctx, newRG(managed, "worker", "data", "writer")); err != nil {
		t.Fatalf("portal create writer grant: %v", err)
	}
	bc := &idpv1alpha1.BucketClaim{}
	if err := pc.Get(ctx, types.NamespacedName{Namespace: managed, Name: "data"}, bc); err != nil {
		t.Fatalf("portal get bucketclaim: %v", err)
	}
	bl := &idpv1alpha1.BucketClaimList{}
	if err := pc.List(ctx, bl, client.InNamespace(managed)); err != nil || len(bl.Items) != 1 {
		t.Fatalf("portal list bucketclaims: %v %d", err, len(bl.Items))
	}
	rl := &idpv1alpha1.ResourceGrantList{}
	if err := pc.List(ctx, rl, client.InNamespace(managed)); err != nil || len(rl.Items) != 2 {
		t.Fatalf("portal list resourcegrants: %v %d", err, len(rl.Items))
	}
	if err := pc.Delete(ctx, newRG(managed, "worker", "data", "writer")); err != nil {
		t.Fatalf("portal revoke (delete) a resourcegrant in managed ns: %v", err)
	}
	rg := &idpv1alpha1.ResourceGrant{}
	if err := pc.Get(ctx, types.NamespacedName{Namespace: managed, Name: "orders-data"}, rg); err != nil {
		t.Fatalf("portal get resourcegrant: %v", err)
	}

	// Denied: an unmanaged namespace (create, and delete of a grant that exists there).
	wantPortal("create bucketclaim in unlabelled ns", pc.Create(ctx, newBC(plain, "data"), dry))
	wantPortal("create bucketclaim in kube-system", pc.Create(ctx, newBC("kube-system", "data"), dry))
	wantPortal("create bucketclaim in idp-system", pc.Create(ctx, newBC("idp-system", "data"), dry))
	wantPortal("create resourcegrant in unlabelled ns", pc.Create(ctx, newRG(plain, "orders", "data", "reader"), dry))
	if err := k8s.Create(ctx, newRG(plain, "orders", "data", "reader")); err != nil {
		t.Fatal(err)
	}
	wantPortal("delete resourcegrant in unlabelled ns", pc.Delete(ctx, newRG(plain, "orders", "data", "reader"), dry))

	// Denied: labels, annotations, finalizers, ownerReferences, on both kinds.
	owner := []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "x", UID: "00000000-0000-0000-0000-000000000005"}}
	for kind, mk := range map[string]func(string) client.Object{
		"bucketclaim":   func(n string) client.Object { return newBC(managed, n) },
		"resourcegrant": func(n string) client.Object { return newRG(managed, n, "data", "reader") },
	} {
		o := mk("labelled")
		o.SetLabels(map[string]string{"x": "y"})
		wantPortal(kind+" with a label", pc.Create(ctx, o, dry))
		o = mk("annotated")
		o.SetAnnotations(map[string]string{"x": "y"})
		wantPortal(kind+" with an annotation", pc.Create(ctx, o, dry))
		o = mk("fin")
		o.SetFinalizers([]string{"example.com/block"})
		wantPortal(kind+" with a finalizer", pc.Create(ctx, o, dry))
		o = mk("owned")
		o.SetOwnerReferences(owner)
		wantPortal(kind+" with an ownerReference", pc.Create(ctx, o, dry))
		// the broker's own finalizers are the broker's to add, not the portal's
		o = mk("brokerfin")
		o.SetFinalizers([]string{"idp.ithaca-idp.com/bucket-release", "idp.ithaca-idp.com/grant-release"})
		wantPortal(kind+" with the broker's finalizer", pc.Create(ctx, o, dry))
	}

	// Denied: a grant whose name is not <workload>-<resource>, even when the spec is fine.
	for _, name := range []string{"anything", "orders", "orders-other", "data-orders", "orders-data-x"} {
		g := newRG(managed, "orders", "data", "reader")
		g.Name = name
		wantPortal("grant named "+name, pc.Create(ctx, g, dry))
	}

	// Denied: names Cloud Storage refuses (the CRD allows them, the broker answers InvalidName).
	for _, n := range []string{"my-google", "google", "goog-data", "googly"} {
		wantPortal("bucketclaim "+n, pc.Create(ctx, newBC(managed, n), dry))
	}
	// Denied: a namespace of more than 31 characters, or one with goog/google: the broker cannot
	// name a bucket for it. (A 31-character one is allowed.)
	long, goog := strings.Repeat("l", 32), "goog-res-portal"
	ok31 := strings.Repeat("m", 31)
	for _, ns := range []string{long, goog, ok31} {
		createAndReconcile(t, newTN(ns))
	}
	wantPortal("bucketclaim in a 32-character namespace", pc.Create(ctx, newBC(long, "data"), dry))
	wantPortal("bucketclaim in a goog namespace", pc.Create(ctx, newBC(goog, "data"), dry))
	if err := pc.Create(ctx, newBC(ok31, "data"), dry); err != nil {
		t.Errorf("bucketclaim in a 31-character namespace: %v", err)
	}

	// Refused by the CRD before the policy (CEL on the name, maxLength, enum, DNS label), so only
	// "not accepted" is asserted: a name over 18, upper case, a bad access, a malformed workload.
	for _, n := range []string{strings.Repeat("a", 19), "Upper", "a.b", "-a"} {
		if err := pc.Create(ctx, newBC(managed, n), dry); err == nil {
			t.Errorf("bucketclaim name %q accepted", n)
		}
	}
	for _, access := range []string{"admin", "", "Reader", "owner"} {
		if err := pc.Create(ctx, newRG(managed, "orders", "data", access), dry); err == nil {
			t.Errorf("access %q accepted", access)
		}
	}
	for _, g := range []*idpv1alpha1.ResourceGrant{
		newRG(managed, "Orders", "data", "reader"), newRG(managed, "kube-x", "data", "reader"),
		newRG(managed, "orders", strings.Repeat("a", 19), "reader"), newRG(managed, strings.Repeat("w", 41), "data", "reader"),
	} {
		if err := pc.Create(ctx, g, dry); err == nil {
			t.Errorf("grant %s accepted", g.Name)
		}
	}

	// Extra spec fields are pruned: a claim takes nothing but a name, a grant only its three.
	extraBC := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": idpv1alpha1.GroupVersion.String(), "kind": "BucketClaim",
		"metadata": map[string]any{"name": "pruned", "namespace": managed},
		"spec":     map[string]any{"location": "EU", "versioning": true, "bucketName": "idp-other-0000000a"},
	}}
	if err := pc.Create(ctx, extraBC, dry); err != nil {
		t.Fatal(err)
	} else if spec, _, _ := unstructured.NestedMap(extraBC.Object, "spec"); len(spec) != 0 {
		t.Errorf("extra bucketclaim spec fields stored: %v", spec)
	}
	extraRG := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": idpv1alpha1.GroupVersion.String(), "kind": "ResourceGrant",
		"metadata": map[string]any{"name": "orders-data2", "namespace": managed},
		"spec":     map[string]any{"workload": "orders", "resource": "data2", "access": "reader", "role": "roles/storage.admin", "member": "allUsers"},
	}}
	if err := pc.Create(ctx, extraRG, dry); err != nil {
		t.Fatal(err)
	} else if spec, _, _ := unstructured.NestedMap(extraRG.Object, "spec"); len(spec) != 3 {
		t.Errorf("resourcegrant spec after pruning: %v", spec)
	}

	// RBAC: no update, patch, status write or watch on either kind, no bucketclaim delete, no
	// deletecollection; RBAC refuses first, and the next test shows the policy would too.
	wantForbidden(t, "delete bucketclaim", pc.Delete(ctx, newBC(managed, "data"), dry))
	wantForbidden(t, "deletecollection bucketclaims", pc.DeleteAllOf(ctx, &idpv1alpha1.BucketClaim{}, client.InNamespace(managed), dry))
	wantForbidden(t, "deletecollection resourcegrants", pc.DeleteAllOf(ctx, &idpv1alpha1.ResourceGrant{}, client.InNamespace(managed), dry))
	upBC := bc.DeepCopy()
	upBC.Labels = map[string]string{"x": "y"}
	wantForbidden(t, "update bucketclaim", pc.Update(ctx, upBC, dry))
	wantForbidden(t, "patch bucketclaim", pc.Patch(ctx, upBC, client.MergeFrom(bc), dry))
	upRG := rg.DeepCopy()
	upRG.Labels = map[string]string{"x": "y"}
	wantForbidden(t, "update resourcegrant", pc.Update(ctx, upRG, dry))
	wantForbidden(t, "patch resourcegrant", pc.Patch(ctx, upRG, client.MergeFrom(rg), dry))
	stBC := bc.DeepCopy()
	stBC.Status.BucketName = "idp-evil-0000000a"
	wantForbidden(t, "update bucketclaim status", pc.Status().Update(ctx, stBC, dry))
	stRG := rg.DeepCopy()
	stRG.Status.Member = "allUsers"
	wantForbidden(t, "update resourcegrant status", pc.Status().Update(ctx, stRG, dry))
	wantForbidden(t, "create ServiceAccount", pc.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "sa", Namespace: managed}}, dry))
}

// TestPortalResourcesPolicyDeniesWhatRBACWouldAllow gives the portal a ClusterRole that is too
// wide (delete on bucketclaims, update on both kinds, as if idp-gitops were changed by mistake)
// and shows that the policy alone still refuses those writes.
func TestPortalResourcesPolicyDeniesWhatRBACWouldAllow(t *testing.T) {
	pc := portalAPIClient(t)
	managed := "team-res-wide"
	createAndReconcile(t, newTN(managed))
	if err := pc.Create(ctx, newBC(managed, "data")); err != nil {
		t.Fatal(err)
	}
	if err := pc.Create(ctx, newRG(managed, "orders", "data", "reader")); err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "idp-portal-too-wide"}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{idpv1alpha1.GroupVersion.Group}, Resources: []string{"bucketclaims", "resourcegrants", "bucketclaims/status", "resourcegrants/status"},
			Verbs: []string{"update", "patch", "delete", "deletecollection"}},
	}}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "idp-portal-too-wide"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "portal-api", Namespace: "idp-system"}},
	}
	for _, o := range []client.Object{role, binding} {
		if err := k8s.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8s.Delete(ctx, o) })
	}
	dry := client.DryRunAll
	eventually(t, "delete of a bucketclaim reaches the policy", func() error {
		if err := pc.Delete(ctx, newBC(managed, "data"), dry); !isPortalDenial(err) {
			return err
		}
		return nil
	})
	wantPolicy := func(what string, err error) {
		t.Helper()
		if !isPortalDenial(err) {
			t.Errorf("%s: want denial by %s, got %v", what, portalPolicyName, err)
		}
	}
	bc := &idpv1alpha1.BucketClaim{}
	if err := pc.Get(ctx, types.NamespacedName{Namespace: managed, Name: "data"}, bc); err != nil {
		t.Fatal(err)
	}
	bc.Labels = map[string]string{"x": "y"}
	wantPolicy("update bucketclaim", pc.Update(ctx, bc, dry))
	rg := &idpv1alpha1.ResourceGrant{}
	if err := pc.Get(ctx, types.NamespacedName{Namespace: managed, Name: "orders-data"}, rg); err != nil {
		t.Fatal(err)
	}
	rg.Labels = map[string]string{"x": "y"}
	wantPolicy("update resourcegrant", pc.Update(ctx, rg, dry))
	wantPolicy("deletecollection bucketclaims", pc.DeleteAllOf(ctx, &idpv1alpha1.BucketClaim{}, client.InNamespace(managed), dry))
	st := rg.DeepCopy()
	st.Status.Member = "allUsers"
	wantPolicy("update resourcegrant status", pc.Status().Update(ctx, st, dry))
}
