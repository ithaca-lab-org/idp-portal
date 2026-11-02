package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

var testWIOptions = WorkloadIdentityOptions{TrustDomain: DefaultTrustDomain, HostnameSuffix: DefaultHostnameSuffix}

func newWI(ns, name string) *idpv1alpha1.WorkloadIdentity {
	return &idpv1alpha1.WorkloadIdentity{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

func reconcileWI(t *testing.T, c client.Client, o *VaultOptions, ns, name string) ctrl.Result {
	t.Helper()
	r := &WorkloadIdentityReconciler{Client: c, Scheme: testScheme, Options: testWIOptions, Vault: o}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	if err != nil {
		t.Fatalf("reconcile workloadidentity %s/%s: %v", ns, name, err)
	}
	return res
}

func getU(gvk schema.GroupVersionKind, ns, name string) (*unstructured.Unstructured, error) {
	u := newUnstructured(gvk, "", "")
	return u, k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, u)
}

func mustGetU(t *testing.T, gvk schema.GroupVersionKind, ns, name string) *unstructured.Unstructured {
	t.Helper()
	u, err := getU(gvk, ns, name)
	if err != nil {
		t.Fatalf("get %s %s/%s: %v", gvk.Kind, ns, name, err)
	}
	return u
}

func wiSynced(t *testing.T, ns, name string) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(get(t, &idpv1alpha1.WorkloadIdentity{}, ns, name).Status.Conditions, idpv1alpha1.ConditionSynced)
}

// wiOwned gives obj the operator's labels and an owner reference to a
// (made-up) WorkloadIdentity of the same name.
func wiOwned[T client.Object](obj T) T {
	obj.SetLabels(map[string]string{LabelManagedBy: ManagedByValue, LabelWorkload: obj.GetName()})
	obj.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: idpv1alpha1.GroupVersion.String(), Kind: "WorkloadIdentity", Name: obj.GetName(),
		UID: "00000000-0000-0000-0000-000000000003", Controller: ptr.To(true),
	}})
	return obj
}

func routeFor(ns, name string) *unstructured.Unstructured {
	u := newUnstructured(HTTPRouteGVK, ns, name)
	u.Object["spec"] = desiredHTTPRouteSpec(testWIOptions, ns, name)
	return wiOwned(u)
}

func policyFor(ns, name string) *unstructured.Unstructured {
	u := newUnstructured(AuthorizationPolicyGVK, ns, name)
	u.Object["spec"] = desiredAuthorizationPolicySpec(testWIOptions, ns, name)
	return wiOwned(u)
}

func wiSA(ns, name string) *corev1.ServiceAccount {
	return wiOwned(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		AutomountServiceAccountToken: ptr.To(false)})
}

func TestWorkloadIdentityOptionsValidate(t *testing.T) {
	if err := testWIOptions.Validate(); err != nil {
		t.Errorf("defaults: %v", err)
	}
	for _, o := range []WorkloadIdentityOptions{
		{TrustDomain: "", HostnameSuffix: DefaultHostnameSuffix},
		{TrustDomain: "Cluster.Local", HostnameSuffix: DefaultHostnameSuffix},
		{TrustDomain: "td/ns/x", HostnameSuffix: DefaultHostnameSuffix},
		{TrustDomain: DefaultTrustDomain, HostnameSuffix: ""},
		{TrustDomain: DefaultTrustDomain, HostnameSuffix: "*.apps.example.com"},
		{TrustDomain: DefaultTrustDomain, HostnameSuffix: "apps.example.com."},
	} {
		if err := o.Validate(); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
}

func TestWorkloadIdentityCreatesChildren(t *testing.T) {
	requireEnvtest(t)
	ns, name := "team-wi", "payments-api"
	vaultNamespace(t, k8s, ns)
	if err := k8s.Create(ctx, newWI(ns, name)); err != nil {
		t.Fatal(err)
	}
	reconcileWI(t, k8s, testVaultOptions(), ns, name)
	wi := get(t, &idpv1alpha1.WorkloadIdentity{}, ns, name)

	sa := get(t, &corev1.ServiceAccount{}, ns, name)
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Error("ServiceAccount should not automount its token")
	}
	route := mustGetU(t, HTTPRouteGVK, ns, name)
	wantRoute := map[string]any{
		"parentRefs": []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway",
			"namespace": "istio-ingress", "name": "ingress", "sectionName": "https"}},
		"hostnames": []any{"payments-api.team-wi.apps.ithaca-idp.com"},
		"rules": []any{map[string]any{"matches": []any{map[string]any{
			"path": map[string]any{"type": "PathPrefix", "value": "/"}}}}},
	}
	if got := route.Object["spec"]; !equality.Semantic.DeepEqual(got, wantRoute) {
		t.Errorf("HTTPRoute spec = %#v\nwant %#v", got, wantRoute)
	}
	ap := mustGetU(t, AuthorizationPolicyGVK, ns, name)
	wantAP := map[string]any{
		"selector": map[string]any{"matchLabels": map[string]any{"idp.ithaca-idp.com/workload": name}},
		"action":   "ALLOW",
		"rules": []any{map[string]any{"from": []any{
			map[string]any{"source": map[string]any{"namespaces": []any{ns}}},
			map[string]any{"source": map[string]any{"principals": []any{"cluster.local/ns/istio-ingress/sa/ingress-istio"}}},
		}}},
	}
	if got := ap.Object["spec"]; !equality.Semantic.DeepEqual(got, wantAP) {
		t.Errorf("AuthorizationPolicy spec = %#v\nwant %#v", got, wantAP)
	}
	va := mustGetU(t, VaultAuthGVK, ns, name)
	wantVA := map[string]any{
		"method": "kubernetes", "mount": "kubernetes", "vaultConnectionRef": "vault-secrets-operator/idp-vault",
		"kubernetes": map[string]any{"role": "workload-reader", "serviceAccount": name,
			"audiences": []any{"vault"}, "tokenExpirationSeconds": int64(600)},
	}
	if got := va.Object["spec"]; !equality.Semantic.DeepEqual(got, wantVA) {
		t.Errorf("VaultAuth spec = %#v\nwant %#v", got, wantVA)
	}
	for _, o := range []client.Object{sa, route, ap, va} {
		if !metav1.IsControlledBy(o, wi) || len(o.GetOwnerReferences()) != 1 {
			t.Errorf("%T owners = %+v, want only the WorkloadIdentity as controller", o, o.GetOwnerReferences())
		}
		if l := o.GetLabels(); l[LabelManagedBy] != ManagedByValue || l[LabelWorkload] != name {
			t.Errorf("%T labels = %v", o, l)
		}
	}
	if wi.Status.SpiffeID != "spiffe://cluster.local/ns/team-wi/sa/payments-api" ||
		wi.Status.Hostname != "payments-api.team-wi.apps.ithaca-idp.com" || wi.Status.ObservedGeneration != wi.Generation {
		t.Errorf("status = %+v", wi.Status)
	}
	if c := wiSynced(t, ns, name); c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonSynced {
		t.Errorf("Synced = %+v", c)
	}

	// A second pass changes nothing (the defaulted HTTPRoute compares equal).
	rvs := map[string]string{"sa": sa.ResourceVersion, "route": route.GetResourceVersion(), "ap": ap.GetResourceVersion(),
		"va": va.GetResourceVersion(), "wi": wi.ResourceVersion}
	reconcileWI(t, k8s, testVaultOptions(), ns, name)
	for k, got := range map[string]string{
		"sa": get(t, &corev1.ServiceAccount{}, ns, name).ResourceVersion, "route": mustGetU(t, HTTPRouteGVK, ns, name).GetResourceVersion(),
		"ap": mustGetU(t, AuthorizationPolicyGVK, ns, name).GetResourceVersion(), "va": mustGetU(t, VaultAuthGVK, ns, name).GetResourceVersion(),
		"wi": get(t, &idpv1alpha1.WorkloadIdentity{}, ns, name).ResourceVersion,
	} {
		if got != rvs[k] {
			t.Errorf("second reconcile wrote %s", k)
		}
	}

	// Drift is reverted: a backend on the route, an open policy, another role.
	_ = unstructured.SetNestedSlice(route.Object, []any{map[string]any{"backendRefs": []any{
		map[string]any{"name": "svc", "port": int64(80)}}}}, "spec", "rules")
	_ = unstructured.SetNestedField(ap.Object, "DENY", "spec", "action")
	ap.SetAnnotations(map[string]string{"istio.io/dry-run": "true"})
	_ = unstructured.SetNestedField(va.Object, "ns-reader", "spec", "kubernetes", "role")
	for _, o := range []client.Object{route, ap, va} {
		if err := k8s.Update(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	reconcileWI(t, k8s, testVaultOptions(), ns, name)
	for gvk, want := range map[schema.GroupVersionKind]map[string]any{HTTPRouteGVK: wantRoute, AuthorizationPolicyGVK: wantAP, VaultAuthGVK: wantVA} {
		if got := mustGetU(t, gvk, ns, name).Object["spec"]; !equality.Semantic.DeepEqual(got, want) {
			t.Errorf("%s drift not reverted: %#v", gvk.Kind, got)
		}
	}
	if a := mustGetU(t, AuthorizationPolicyGVK, ns, name).GetAnnotations(); len(a) != 0 {
		t.Errorf("AuthorizationPolicy annotations not dropped: %v", a)
	}
	if !slices.Contains(get(t, &idpv1alpha1.WorkloadIdentity{}, ns, name).Finalizers, WorkloadVaultFinalizerName) {
		t.Error("Vault purge finalizer missing with Vault on")
	}
}

func TestWorkloadIdentityVaultDisabled(t *testing.T) {
	requireEnvtest(t)
	ns, name := "team-wi-novault", "api"
	createAndReconcile(t, newTN(ns))
	if err := k8s.Create(ctx, newWI(ns, name)); err != nil {
		t.Fatal(err)
	}
	reconcileWI(t, k8s, nil, ns, name)
	if _, err := getU(VaultAuthGVK, ns, name); !apierrors.IsNotFound(err) {
		t.Errorf("VaultAuth created with Vault disabled: %v", err)
	}
	if f := get(t, &idpv1alpha1.WorkloadIdentity{}, ns, name).Finalizers; len(f) != 0 {
		t.Errorf("finalizers with Vault disabled: %v", f)
	}
	get(t, &corev1.ServiceAccount{}, ns, name)
	mustGetU(t, HTTPRouteGVK, ns, name)
	mustGetU(t, AuthorizationPolicyGVK, ns, name)
	if c := wiSynced(t, ns, name); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Synced = %+v", c)
	}
}

// A child of the same name that the WorkloadIdentity does not own (made by
// hand here) is never adopted or changed.
func TestWorkloadIdentityNameConflict(t *testing.T) {
	requireEnvtest(t)
	ns := "team-wi-conflict"
	createAndReconcile(t, newTN(ns))
	handSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "api"}}
	handRoute := routeFor(ns, "web")
	handRoute.SetOwnerReferences(nil)
	for _, o := range []client.Object{handSA, handRoute} {
		if err := k8s.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"api", "web"} {
		if err := k8s.Create(ctx, newWI(ns, name)); err != nil {
			t.Fatal(err)
		}
		if res := reconcileWI(t, k8s, nil, ns, name); res.RequeueAfter < time.Minute {
			t.Errorf("%s: RequeueAfter = %v, want a slow requeue", name, res.RequeueAfter)
		}
		if c := wiSynced(t, ns, name); c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonNameConflict {
			t.Errorf("%s: Synced = %+v", name, c)
		}
	}
	if got := get(t, &corev1.ServiceAccount{}, ns, "api"); got.ResourceVersion != handSA.ResourceVersion || len(got.OwnerReferences) != 0 {
		t.Errorf("hand-made ServiceAccount was modified: %+v", got.ObjectMeta)
	}
	if got := mustGetU(t, HTTPRouteGVK, ns, "web"); got.GetResourceVersion() != handRoute.GetResourceVersion() || len(got.GetOwnerReferences()) != 0 {
		t.Errorf("hand-made HTTPRoute was modified: owners %+v", got.GetOwnerReferences())
	}
	// Children from an earlier WorkloadIdentity of the same name are waited for.
	old := wiSA(ns, "prev")
	if err := k8s.Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(ctx, newWI(ns, "prev")); err != nil {
		t.Fatal(err)
	}
	if res := reconcileWI(t, k8s, nil, ns, "prev"); res.RequeueAfter == 0 || res.RequeueAfter > time.Minute {
		t.Errorf("previous owner: RequeueAfter = %v, want a short requeue", res.RequeueAfter)
	}
	if c := wiSynced(t, ns, "prev"); c == nil || c.Reason != ReasonPreviousTerminating {
		t.Errorf("Synced = %+v", c)
	}
}

func TestWorkloadIdentityIgnoredOutsideManagedNamespaces(t *testing.T) {
	requireEnvtest(t)
	plain := "plain-wi"
	ensurePlainNamespace(t, plain)
	fake := "fake-managed-wi"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fake, Labels: map[string]string{
		LabelManagedBy: ManagedByValue, LabelTeamNamespace: fake,
	}}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	for _, ns := range []string{plain, fake, "kube-system"} {
		if err := k8s.Create(ctx, newWI(ns, "api")); err != nil {
			t.Fatal(err)
		}
		reconcileWI(t, k8s, testVaultOptions(), ns, "api")
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "api"}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
			t.Errorf("%s: ServiceAccount created outside a managed namespace: %v", ns, err)
		}
		for _, gvk := range []schema.GroupVersionKind{HTTPRouteGVK, AuthorizationPolicyGVK, VaultAuthGVK} {
			if _, err := getU(gvk, ns, "api"); !apierrors.IsNotFound(err) {
				t.Errorf("%s: %s created outside a managed namespace: %v", ns, gvk.Kind, err)
			}
		}
		if c := wiSynced(t, ns, "api"); c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonNamespaceNotManaged {
			t.Errorf("%s: Synced = %+v", ns, c)
		}
	}
}

// envtest runs no garbage collector; cleanup relies on the controller owner
// references checked in TestWorkloadIdentityCreatesChildren. Here: no
// finalizer, so the WorkloadIdentity goes at once, and reconciling it
// afterwards writes nothing.
func TestWorkloadIdentityDeletion(t *testing.T) {
	requireEnvtest(t)
	ns := "team-wi-del"
	createAndReconcile(t, newTN(ns))
	if err := k8s.Create(ctx, newWI(ns, "api")); err != nil {
		t.Fatal(err)
	}
	reconcileWI(t, k8s, nil, ns, "api")
	if len(get(t, &idpv1alpha1.WorkloadIdentity{}, ns, "api").Finalizers) != 0 {
		t.Error("WorkloadIdentity has finalizers")
	}
	if err := k8s.Delete(ctx, newWI(ns, "api")); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "api"}, &idpv1alpha1.WorkloadIdentity{}); !apierrors.IsNotFound(err) {
		t.Fatalf("WorkloadIdentity still present: %v", err)
	}
	rv := mustGetU(t, HTTPRouteGVK, ns, "api").GetResourceVersion()
	reconcileWI(t, k8s, nil, ns, "api")
	if mustGetU(t, HTTPRouteGVK, ns, "api").GetResourceVersion() != rv {
		t.Error("reconcile of a deleted WorkloadIdentity touched the HTTPRoute")
	}
}

func TestWorkloadIdentityCRDValidation(t *testing.T) {
	requireEnvtest(t)
	ns := "plain-wi-crd"
	ensurePlainNamespace(t, ns)
	dry := client.DryRunAll
	for _, n := range []string{"vault-reader", "default", "default-token-abcde", "vault-reader-token-x", "sh-helm-x",
		"kube-x", "istio-x", "idp-quota", strings.Repeat("a", 41), "Upper", "a.b", "-a"} {
		if err := k8s.Create(ctx, newWI(ns, n), dry); err == nil {
			t.Errorf("name %q accepted", n)
		}
	}
	if err := k8s.Create(ctx, newWI(ns, strings.Repeat("a", 40)), dry); err != nil {
		t.Errorf("40-char name rejected: %v", err)
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": idpv1alpha1.GroupVersion.String(), "kind": "WorkloadIdentity",
		"metadata": map[string]any{"name": "extra", "namespace": ns},
		"spec":     map[string]any{"hostname": "portal.apps.ithaca-idp.com", "serviceAccount": "default"},
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

// TestOperatorWorkloadIdentityUnderRBACAndPolicy runs the reconciler as the
// operator's ServiceAccount and checks that the policy pins the children.
func TestOperatorWorkloadIdentityUnderRBACAndPolicy(t *testing.T) {
	op := operatorClient(t)
	managed, name := "team-wi-rbac", "api"
	vaultNamespace(t, op, managed)
	if err := k8s.Create(ctx, newWI(managed, name)); err != nil {
		t.Fatal(err)
	}
	reconcileWI(t, op, testVaultOptions(), managed, name)
	if c := wiSynced(t, managed, name); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Synced = %+v", c)
	}
	// Re-reconcile after drift as the operator (update path under the policy).
	route := mustGetU(t, HTTPRouteGVK, managed, name)
	_ = unstructured.SetNestedStringSlice(route.Object, []string{"evil.apps.ithaca-idp.com"}, "spec", "hostnames")
	if err := k8s.Update(ctx, route); err != nil {
		t.Fatal(err)
	}
	reconcileWI(t, op, testVaultOptions(), managed, name)

	plain := "plain-ns"
	ensurePlainNamespace(t, plain)
	dry := client.DryRunAll

	for what, o := range map[string]client.Object{
		"HTTPRoute in kube-system":            routeFor("kube-system", "x"),
		"HTTPRoute in unlabelled ns":          routeFor(plain, "x"),
		"AuthorizationPolicy in unlabelled":   policyFor(plain, "x"),
		"ServiceAccount in unlabelled":        wiSA(plain, "x"),
		"ServiceAccount named default":        wiSA(managed, "default"),
		"ServiceAccount named kube-x":         wiSA(managed, "kube-x"),
		"ServiceAccount with token automount": func() client.Object { s := wiSA(managed, "auto"); s.AutomountServiceAccountToken = nil; return s }(),
		"ServiceAccount with a Secret": func() client.Object {
			s := wiSA(managed, "sec")
			s.Secrets = []corev1.ObjectReference{{Name: "x"}}
			return s
		}(),
		"AuthorizationPolicy with istio.io/dry-run": func() client.Object {
			u := policyFor(managed, "dry")
			u.SetAnnotations(map[string]string{"istio.io/dry-run": "true"})
			return u
		}(),
		"HTTPRoute without owner": func() client.Object { u := routeFor(managed, "orphan"); u.SetOwnerReferences(nil); return u }(),
		"AuthorizationPolicy owned by ManagedSecret": func() client.Object {
			u := policyFor(managed, "msowned")
			refs := u.GetOwnerReferences()
			refs[0].Kind = "ManagedSecret"
			u.SetOwnerReferences(refs)
			return u
		}(),
		"VaultAuth without owner": func() client.Object {
			u := newVaultAuthNamed(managed, "orphan")
			u.Object["spec"] = vaultAuthSpec(WorkloadVaultRole, "orphan")
			return u
		}(),
	} {
		wantPolicyDenial(t, "create "+what, op.Create(ctx, o, dry))
	}

	nsSrc := map[string]any{"source": map[string]any{"namespaces": []any{managed}}}
	gwSrc := map[string]any{"source": map[string]any{"principals": []any{"cluster.local/ns/istio-ingress/sa/ingress-istio"}}}
	fromRules := func(from ...any) []any { return []any{map[string]any{"from": from}} }
	for what, tc := range map[string]struct {
		gvk    schema.GroupVersionKind
		mutate func(u *unstructured.Unstructured)
	}{
		"route hostname of another namespace": {HTTPRouteGVK, func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedStringSlice(u.Object, []string{"api.team-other.apps.ithaca-idp.com"}, "spec", "hostnames")
		}},
		"route other suffix": {HTTPRouteGVK, func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedStringSlice(u.Object, []string{"api.team-wi-rbac.evil.example.com"}, "spec", "hostnames")
		}},
		"policy dry-run annotation": {AuthorizationPolicyGVK, func(u *unstructured.Unstructured) {
			u.SetAnnotations(map[string]string{"istio.io/dry-run": "true"})
		}},
		"route annotation": {HTTPRouteGVK, func(u *unstructured.Unstructured) {
			u.SetAnnotations(map[string]string{"x": "y"})
		}},
		"route platform hostname": {HTTPRouteGVK, func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedStringSlice(u.Object, []string{"portal.apps.ithaca-idp.com"}, "spec", "hostnames")
		}},
		"route second hostname": {HTTPRouteGVK, func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedStringSlice(u.Object, []string{"api.team-wi-rbac.apps.ithaca-idp.com", "x.team-wi-rbac.apps.ithaca-idp.com"}, "spec", "hostnames")
		}},
		"route http listener": {HTTPRouteGVK, func(u *unstructured.Unstructured) {
			p := desiredHTTPRouteSpec(testWIOptions, managed, name)["parentRefs"].([]any)[0].(map[string]any)
			p["sectionName"] = "http"
			_ = unstructured.SetNestedSlice(u.Object, []any{p}, "spec", "parentRefs")
		}},
		"route other gateway": {HTTPRouteGVK, func(u *unstructured.Unstructured) {
			p := desiredHTTPRouteSpec(testWIOptions, managed, name)["parentRefs"].([]any)[0].(map[string]any)
			p["namespace"], p["name"] = "keycloak", "internal"
			_ = unstructured.SetNestedSlice(u.Object, []any{p}, "spec", "parentRefs")
		}},
		"route backend": {HTTPRouteGVK, func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"backendRefs": []any{
				map[string]any{"name": "portal-api", "namespace": "idp-system", "port": int64(80)}}}}, "spec", "rules")
		}},
		"route redirect filter": {HTTPRouteGVK, func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"filters": []any{map[string]any{
				"type": "RequestRedirect", "requestRedirect": map[string]any{"hostname": "evil.example.com"}}}}}, "spec", "rules")
		}},
		"policy AUDIT action": {AuthorizationPolicyGVK, func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "AUDIT", "spec", "action")
		}},
		"policy other selector": {AuthorizationPolicyGVK, func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedStringMap(u.Object, map[string]string{"app": "db"}, "spec", "selector", "matchLabels")
		}},
		"policy targetRefs instead of selector": {AuthorizationPolicyGVK, func(u *unstructured.Unstructured) {
			unstructured.RemoveNestedField(u.Object, "spec", "selector")
			_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"kind": "Service", "group": "", "name": "x"}}, "spec", "targetRefs")
		}},
		"policy wildcard principal": {AuthorizationPolicyGVK, func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(u.Object, fromRules(nsSrc, map[string]any{"source": map[string]any{"principals": []any{"*"}}}), "spec", "rules")
		}},
		"policy other principal": {AuthorizationPolicyGVK, func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(u.Object, fromRules(nsSrc, map[string]any{"source": map[string]any{"principals": []any{"cluster.local/ns/team-x/sa/x"}}}), "spec", "rules")
		}},
		"policy other namespace": {AuthorizationPolicyGVK, func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(u.Object, fromRules(map[string]any{"source": map[string]any{"namespaces": []any{"*"}}}, gwSrc), "spec", "rules")
		}},
		"policy empty source": {AuthorizationPolicyGVK, func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(u.Object, fromRules(nsSrc, map[string]any{"source": map[string]any{}}), "spec", "rules")
		}},
		"policy third source": {AuthorizationPolicyGVK, func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(u.Object, fromRules(nsSrc, gwSrc, map[string]any{"source": map[string]any{"ipBlocks": []any{"0.0.0.0/0"}}}), "spec", "rules")
		}},
		"policy ipBlocks": {AuthorizationPolicyGVK, func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(u.Object, fromRules(nsSrc, map[string]any{"source": map[string]any{
				"principals": []any{"cluster.local/ns/istio-ingress/sa/ingress-istio"}, "ipBlocks": []any{"0.0.0.0/0"}}}), "spec", "rules")
		}},
		"policy second rule": {AuthorizationPolicyGVK, func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(u.Object, append(fromRules(nsSrc, gwSrc), map[string]any{}), "spec", "rules")
		}},
		"vaultauth role ns-reader": {VaultAuthGVK, func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "ns-reader", "spec", "kubernetes", "role")
		}},
		"vaultauth as vault-reader": {VaultAuthGVK, func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "vault-reader", "spec", "kubernetes", "serviceAccount")
		}},
		"vaultauth purge role": {VaultAuthGVK, func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "idp-operator-purge", "spec", "kubernetes", "role")
		}},
	} {
		u := mustGetU(t, tc.gvk, managed, name)
		tc.mutate(u)
		wantPolicyDenial(t, "update "+what, op.Update(ctx, u, dry))
	}

	// The namespace's vault-reader VaultAuth cannot take the workload role.
	va := getVaultAuth(t, managed)
	_ = unstructured.SetNestedField(va.Object, "workload-reader", "spec", "kubernetes", "role")
	wantPolicyDenial(t, "vault-reader with role workload-reader", op.Update(ctx, va, dry))

	// ServiceAccounts are create-only; nothing is deleted by the operator.
	sa := get(t, &corev1.ServiceAccount{}, managed, name)
	sa.Labels["x"] = "y"
	wantForbidden(t, "update workload ServiceAccount", op.Update(ctx, sa, dry))
	wantForbidden(t, "delete workload ServiceAccount", op.Delete(ctx, sa, dry))
	for _, gvk := range []schema.GroupVersionKind{HTTPRouteGVK, AuthorizationPolicyGVK, VaultAuthGVK} {
		wantForbidden(t, "delete "+gvk.Kind, op.Delete(ctx, mustGetU(t, gvk, managed, name), dry))
	}

	// The controller reference cannot be moved to another WorkloadIdentity
	// (refused by OwnerReferencesPermissionEnforcement here, by the policy's
	// name check otherwise).
	for _, gvk := range []schema.GroupVersionKind{HTTPRouteGVK, AuthorizationPolicyGVK} {
		u := mustGetU(t, gvk, managed, name)
		refs := u.GetOwnerReferences()
		refs[0].Name = "other"
		u.SetOwnerReferences(refs)
		wantForbidden(t, "swap owner of "+gvk.Kind, op.Update(ctx, u, dry))
	}

	// A hand-made HTTPRoute cannot be adopted or touched.
	hand := routeFor(managed, "handmade")
	hand.SetOwnerReferences(nil)
	if err := k8s.Create(ctx, hand); err != nil {
		t.Fatal(err)
	}
	adopt := mustGetU(t, HTTPRouteGVK, managed, "handmade")
	adopt.SetOwnerReferences(routeFor(managed, "handmade").GetOwnerReferences())
	wantForbidden(t, "adopt hand-made HTTPRoute", op.Update(ctx, adopt, dry))
	touch := mustGetU(t, HTTPRouteGVK, managed, "handmade")
	touch.SetLabels(map[string]string{"x": "y"})
	wantPolicyDenial(t, "update hand-made HTTPRoute", op.Update(ctx, touch, dry))
}

func TestWorkloadIdentityCRDsMissing(t *testing.T) {
	m := meta.NewDefaultRESTMapper(nil)
	missing, err := WorkloadIdentityCRDsMissing(m, true)
	if err != nil || len(missing) != 3 {
		t.Errorf("empty mapper: missing=%v err=%v, want all three", missing, err)
	}
	m.Add(HTTPRouteGVK, meta.RESTScopeNamespace)
	m.Add(AuthorizationPolicyGVK, meta.RESTScopeNamespace)
	if missing, err := WorkloadIdentityCRDsMissing(m, false); err != nil || len(missing) != 0 {
		t.Errorf("Vault off: missing=%v err=%v", missing, err)
	}
	if missing, _ := WorkloadIdentityCRDsMissing(m, true); !slices.Equal(missing, []string{VaultAuthGVK.String()}) {
		t.Errorf("Vault on: missing=%v", missing)
	}
}

// A WorkloadIdentity created before its namespace is managed syncs once the
// TeamNamespace takes the namespace: Namespace/TeamNamespace events map to it.
func TestWorkloadIdentityRequeuedWhenNamespaceBecomesManaged(t *testing.T) {
	requireEnvtest(t)
	ns := "team-wi-late"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(ctx, newWI(ns, "api")); err != nil {
		t.Fatal(err)
	}
	reconcileWI(t, k8s, nil, ns, "api")
	if c := wiSynced(t, ns, "api"); c == nil || c.Reason != ReasonNamespaceNotManaged {
		t.Fatalf("Synced = %+v", c)
	}
	// The TeamNamespace refuses an unlabelled namespace; label it as an admin
	// handing it over would, then reconcile.
	n := get(t, &corev1.Namespace{}, "", ns)
	n.Labels = map[string]string{LabelManagedBy: ManagedByValue, LabelTeamNamespace: ns}
	if err := k8s.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	createAndReconcile(t, newTN(ns))

	r := &WorkloadIdentityReconciler{Client: k8s, Scheme: testScheme, Options: testWIOptions}
	for _, o := range []client.Object{get(t, &corev1.Namespace{}, "", ns), getTN(t, ns)} {
		reqs := r.requestsForNamespace(ctx, o)
		if len(reqs) != 1 || reqs[0].Namespace != ns || reqs[0].Name != "api" {
			t.Errorf("%T maps to %v", o, reqs)
		}
	}
	if reqs := r.requestsForNamespace(ctx, get(t, &corev1.Namespace{}, "", "default")); len(reqs) != 0 {
		t.Errorf("default maps to %v", reqs)
	}
	reconcileWI(t, k8s, nil, ns, "api")
	if c := wiSynced(t, ns, "api"); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Synced after the namespace became managed = %+v", c)
	}
}

// With Vault on, deleting a WorkloadIdentity purges kv/<ns>/<name>/ before the
// finalizer goes; a failing purge keeps it (with VaultPurgePending) until the
// give-up time.
func TestWorkloadIdentityVaultPurgeOnDeletion(t *testing.T) {
	requireEnvtest(t)
	ns := "team-wi-purge"
	vaultNamespace(t, k8s, ns)
	var purged [][]string
	fail := true
	now := time.Now()
	o := testVaultOptions()
	o.Now = func() time.Time { return now }
	o.Purger = purgerFunc(func(_ context.Context, ns string, sub ...string) error {
		purged = append(purged, append([]string{ns}, sub...))
		if fail {
			return errors.New("vault sealed")
		}
		return nil
	})
	for _, name := range []string{"api", "web"} {
		if err := k8s.Create(ctx, newWI(ns, name)); err != nil {
			t.Fatal(err)
		}
		reconcileWI(t, k8s, o, ns, name)
		if err := k8s.Delete(ctx, newWI(ns, name)); err != nil {
			t.Fatal(err)
		}
	}

	// Failing: kept, condition set, requeued.
	res := reconcileWI(t, k8s, o, ns, "api")
	wi := get(t, &idpv1alpha1.WorkloadIdentity{}, ns, "api")
	if res.RequeueAfter == 0 || !slices.Contains(wi.Finalizers, WorkloadVaultFinalizerName) {
		t.Errorf("failing purge: res=%+v finalizers=%v", res, wi.Finalizers)
	}
	if c := meta.FindStatusCondition(wi.Status.Conditions, idpv1alpha1.ConditionVaultPurgePending); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("VaultPurgePending = %+v", c)
	}
	// Past the give-up time: let go.
	now = wi.DeletionTimestamp.Add(o.GiveUpAfter + time.Second)
	reconcileWI(t, k8s, o, ns, "api")
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "api"}, &idpv1alpha1.WorkloadIdentity{}); !apierrors.IsNotFound(err) {
		t.Errorf("WorkloadIdentity still present after give-up: %v", err)
	}
	// Succeeding: purged once, then gone.
	fail, now = false, time.Now()
	reconcileWI(t, k8s, o, ns, "web")
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "web"}, &idpv1alpha1.WorkloadIdentity{}); !apierrors.IsNotFound(err) {
		t.Errorf("WorkloadIdentity still present after purge: %v", err)
	}
	if want := []string{ns, "web"}; !slices.Equal(purged[len(purged)-1], want) {
		t.Errorf("purged %v, want %v last", purged, want)
	}
	for _, p := range purged {
		if len(p) != 2 || p[0] != ns {
			t.Errorf("purge outside kv/%s/<workload>/: %v", ns, p)
		}
	}
}
