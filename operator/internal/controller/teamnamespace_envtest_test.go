package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

var ctx = context.Background()

func createAndReconcile(t *testing.T, tn *idpv1alpha1.TeamNamespace) {
	t.Helper()
	if err := k8s.Create(ctx, tn); err != nil {
		t.Fatalf("create TeamNamespace: %v", err)
	}
	reconcileOnce(t, k8s, tn.Name)
}

func getTN(t *testing.T, name string) *idpv1alpha1.TeamNamespace {
	t.Helper()
	tn := &idpv1alpha1.TeamNamespace{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: name}, tn); err != nil {
		t.Fatalf("get TeamNamespace: %v", err)
	}
	return tn
}

func get[T client.Object](t *testing.T, obj T, ns, name string) T {
	t.Helper()
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, obj); err != nil {
		t.Fatalf("get %T %s/%s: %v", obj, ns, name, err)
	}
	return obj
}

func TestCreatesNamespaceAndGuardrails(t *testing.T) {
	requireEnvtest(t)
	name := "team-create"
	createAndReconcile(t, newTN(name))

	ns := get(t, &corev1.Namespace{}, "", name)
	for k, v := range map[string]string{
		LabelManagedBy:                       "idp-operator",
		LabelTeamNamespace:                   name,
		LabelBusinessUnit:                    "payments",
		"istio.io/dataplane-mode":            "ambient",
		"pod-security.kubernetes.io/enforce": "baseline",
		"pod-security.kubernetes.io/warn":    "restricted",
	} {
		if ns.Labels[k] != v {
			t.Errorf("namespace label %s = %q, want %q", k, ns.Labels[k], v)
		}
	}
	if ns.Annotations[AnnotationOwner] != "dev@example.com" || ns.Annotations[AnnotationOrderID] != "ORD-2026-000123" {
		t.Errorf("namespace annotations = %v", ns.Annotations)
	}

	tn := getTN(t, name)
	q := get(t, &corev1.ResourceQuota{}, name, QuotaName)
	if got := q.Spec.Hard[corev1.ResourceLimitsMemory]; got.Cmp(resource.MustParse("8Gi")) != 0 {
		t.Errorf("small quota limits.memory = %s", got.String())
	}
	if got := q.Spec.Hard[corev1.ResourceServicesLoadBalancers]; !got.IsZero() {
		t.Errorf("loadbalancers quota = %s, want 0", got.String())
	}
	lr := get(t, &corev1.LimitRange{}, name, LimitRangeName)
	if len(lr.Spec.Limits) != 1 || lr.Spec.Limits[0].Type != corev1.LimitTypeContainer {
		t.Errorf("limitrange = %+v", lr.Spec)
	}

	deny := get(t, &networkingv1.NetworkPolicy{}, name, DenyIngressPolicyName)
	if len(deny.Spec.Ingress) != 0 || len(deny.Spec.PodSelector.MatchLabels) != 0 ||
		len(deny.Spec.PolicyTypes) != 1 || deny.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("deny policy = %+v", deny.Spec)
	}
	same := get(t, &networkingv1.NetworkPolicy{}, name, SameNamespacePolicy)
	if len(same.Spec.Ingress) != 1 || len(same.Spec.Ingress[0].From) != 1 ||
		same.Spec.Ingress[0].From[0].PodSelector == nil || same.Spec.Ingress[0].From[0].NamespaceSelector != nil {
		t.Errorf("same-namespace policy = %+v", same.Spec)
	}

	rb := get(t, &rbacv1.RoleBinding{}, name, AdminRoleBindingName)
	if rb.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "admin"}) {
		t.Errorf("roleRef = %+v", rb.RoleRef)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0].Kind != rbacv1.UserKind || rb.Subjects[0].Name != "dev@example.com" {
		t.Errorf("subjects = %+v", rb.Subjects)
	}

	for _, o := range []client.Object{q, lr, deny, same, rb} {
		if !metav1.IsControlledBy(o, tn) || o.GetLabels()[LabelManagedBy] != ManagedByValue {
			t.Errorf("%T %s not controlled/labelled by the TeamNamespace", o, o.GetName())
		}
	}
	if metav1.GetControllerOf(ns) != nil {
		t.Error("namespace must not carry an ownerReference (deletion is label-gated)")
	}

	if tn.Spec.Size != idpv1alpha1.SizeSmall {
		t.Errorf("size default = %q", tn.Spec.Size)
	}
	if !controllerutil.ContainsFinalizer(tn, FinalizerName) {
		t.Error("finalizer missing")
	}
	assertStatus(t, tn, idpv1alpha1.PhaseReady, ReasonReconciled)
	if tn.Status.Namespace != name || tn.Status.ObservedGeneration != tn.Generation {
		t.Errorf("status = %+v", tn.Status)
	}
}

func assertStatus(t *testing.T, tn *idpv1alpha1.TeamNamespace, phase idpv1alpha1.Phase, reason string) {
	t.Helper()
	c := meta.FindStatusCondition(tn.Status.Conditions, idpv1alpha1.ConditionReady)
	if tn.Status.Phase != phase || c == nil || c.Reason != reason {
		t.Fatalf("status phase=%q condition=%+v, want %q/%s", tn.Status.Phase, c, phase, reason)
	}
}

func resourceVersions(t *testing.T, name string) map[string]string {
	t.Helper()
	objs := map[string]client.Object{
		"tn":   &idpv1alpha1.TeamNamespace{},
		"ns":   &corev1.Namespace{},
		"rq":   &corev1.ResourceQuota{},
		"lr":   &corev1.LimitRange{},
		"deny": &networkingv1.NetworkPolicy{},
		"same": &networkingv1.NetworkPolicy{},
		"rb":   &rbacv1.RoleBinding{},
	}
	keys := map[string]types.NamespacedName{
		"tn": {Name: name}, "ns": {Name: name},
		"rq":   {Namespace: name, Name: QuotaName},
		"lr":   {Namespace: name, Name: LimitRangeName},
		"deny": {Namespace: name, Name: DenyIngressPolicyName},
		"same": {Namespace: name, Name: SameNamespacePolicy},
		"rb":   {Namespace: name, Name: AdminRoleBindingName},
	}
	out := map[string]string{}
	for k, o := range objs {
		if err := k8s.Get(ctx, keys[k], o); err != nil {
			t.Fatalf("get %s: %v", k, err)
		}
		out[k] = o.GetResourceVersion()
	}
	return out
}

func TestSecondReconcileIsNoop(t *testing.T) {
	requireEnvtest(t)
	name := "team-idempotent"
	createAndReconcile(t, newTN(name))
	before := resourceVersions(t, name)
	if res := reconcileOnce(t, k8s, name); res.RequeueAfter != 0 {
		t.Errorf("unexpected requeue %+v", res)
	}
	if after := resourceVersions(t, name); !equality.Semantic.DeepEqual(before, after) {
		t.Errorf("second reconcile wrote objects: before=%v after=%v", before, after)
	}
}

func TestSizeChangeUpdatesQuotaAndLimits(t *testing.T) {
	requireEnvtest(t)
	name := "team-resize"
	createAndReconcile(t, newTN(name))

	tn := getTN(t, name)
	tn.Spec.Size = idpv1alpha1.SizeMedium
	if err := k8s.Update(ctx, tn); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, k8s, name)

	q := get(t, &corev1.ResourceQuota{}, name, QuotaName)
	if got := q.Spec.Hard[corev1.ResourceLimitsMemory]; got.Cmp(resource.MustParse("16Gi")) != 0 {
		t.Errorf("medium quota limits.memory = %s", got.String())
	}
	lr := get(t, &corev1.LimitRange{}, name, LimitRangeName)
	if got := lr.Spec.Limits[0].Max[corev1.ResourceMemory]; got.Cmp(resource.MustParse("8Gi")) != 0 {
		t.Errorf("medium limitrange max memory = %s", got.String())
	}
	tn = getTN(t, name)
	if tn.Status.ObservedGeneration != tn.Generation {
		t.Errorf("observedGeneration %d != generation %d", tn.Status.ObservedGeneration, tn.Generation)
	}
}

func TestDriftIsCorrected(t *testing.T) {
	requireEnvtest(t)
	name := "team-drift"
	createAndReconcile(t, newTN(name))

	if err := k8s.Delete(ctx, &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: DenyIngressPolicyName, Namespace: name}}); err != nil {
		t.Fatal(err)
	}
	q := get(t, &corev1.ResourceQuota{}, name, QuotaName)
	q.Spec.Hard[corev1.ResourcePods] = resource.MustParse("999")
	q.Spec.Hard[corev1.ResourceSecrets] = resource.MustParse("1")
	if err := k8s.Update(ctx, q); err != nil {
		t.Fatal(err)
	}
	rb := get(t, &rbacv1.RoleBinding{}, name, AdminRoleBindingName)
	rb.Subjects = append(rb.Subjects, rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "intruder@example.com"})
	if err := k8s.Update(ctx, rb); err != nil {
		t.Fatal(err)
	}
	ns := get(t, &corev1.Namespace{}, "", name)
	ns.Labels["istio.io/dataplane-mode"] = "none"
	ns.Labels["pod-security.kubernetes.io/enforce"] = "privileged"
	ns.Labels["team/extra"] = "kept"
	if err := k8s.Update(ctx, ns); err != nil {
		t.Fatal(err)
	}

	reconcileOnce(t, k8s, name)

	get(t, &networkingv1.NetworkPolicy{}, name, DenyIngressPolicyName)
	q = get(t, &corev1.ResourceQuota{}, name, QuotaName)
	if !equality.Semantic.DeepEqual(q.Spec, desiredQuotaSpec(getTN(t, name))) {
		t.Errorf("quota not converged: %v", q.Spec.Hard)
	}
	rb = get(t, &rbacv1.RoleBinding{}, name, AdminRoleBindingName)
	if len(rb.Subjects) != 1 || rb.Subjects[0].Name != "dev@example.com" {
		t.Errorf("rolebinding subjects not converged: %+v", rb.Subjects)
	}
	ns = get(t, &corev1.Namespace{}, "", name)
	if ns.Labels["istio.io/dataplane-mode"] != "ambient" || ns.Labels["pod-security.kubernetes.io/enforce"] != "baseline" {
		t.Errorf("namespace labels not converged: %v", ns.Labels)
	}
	if ns.Labels["team/extra"] != "kept" {
		t.Error("foreign namespace labels should be preserved")
	}
}

func TestRoleBindingWithForeignRoleRefIsReplaced(t *testing.T) {
	requireEnvtest(t)
	name := "team-roleref"
	createAndReconcile(t, newTN(name))

	if err := k8s.Delete(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: AdminRoleBindingName, Namespace: name}}); err != nil {
		t.Fatal(err)
	}
	bad := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: AdminRoleBindingName, Namespace: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "dev@example.com"}},
	}
	if err := k8s.Create(ctx, bad); err != nil {
		t.Fatal(err)
	}

	if res := reconcileOnce(t, k8s, name); res.RequeueAfter == 0 {
		t.Error("expected a requeue after deleting the drifted rolebinding")
	}
	reconcileOnce(t, k8s, name)
	rb := get(t, &rbacv1.RoleBinding{}, name, AdminRoleBindingName)
	if rb.RoleRef.Name != "admin" {
		t.Errorf("roleRef = %+v", rb.RoleRef)
	}
	assertStatus(t, getTN(t, name), idpv1alpha1.PhaseReady, ReasonReconciled)
}

func TestUnmanagedNamespaceIsNotAdopted(t *testing.T) {
	requireEnvtest(t)
	name := "legacy-team"
	pre := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"owner": "someone-else"}}}
	if err := k8s.Create(ctx, pre); err != nil {
		t.Fatal(err)
	}
	rv := get(t, &corev1.Namespace{}, "", name).ResourceVersion

	createAndReconcile(t, newTN(name))

	tn := getTN(t, name)
	assertStatus(t, tn, idpv1alpha1.PhaseFailed, ReasonNamespaceConflict)
	if controllerutil.ContainsFinalizer(tn, FinalizerName) {
		t.Error("finalizer must not be added for a conflicting namespace")
	}
	if got := get(t, &corev1.Namespace{}, "", name); got.ResourceVersion != rv {
		t.Error("pre-existing namespace was modified")
	}
	var quotas corev1.ResourceQuotaList
	if err := k8s.List(ctx, &quotas, client.InNamespace(name)); err != nil || len(quotas.Items) != 0 {
		t.Errorf("children created in unmanaged namespace: %d (%v)", len(quotas.Items), err)
	}

	// A namespace labelled as managed by a different TeamNamespace is also foreign.
	ns := get(t, &corev1.Namespace{}, "", name)
	ns.Labels[LabelManagedBy] = ManagedByValue
	ns.Labels[LabelTeamNamespace] = "another-team"
	if err := k8s.Update(ctx, ns); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, k8s, name)
	assertStatus(t, getTN(t, name), idpv1alpha1.PhaseFailed, ReasonNamespaceConflict)

	if err := k8s.Delete(ctx, tn); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(ctx, types.NamespacedName{Name: name}, &idpv1alpha1.TeamNamespace{}); !apierrors.IsNotFound(err) {
		t.Errorf("TeamNamespace without finalizer should be gone, got %v", err)
	}
	if got := get(t, &corev1.Namespace{}, "", name); !got.DeletionTimestamp.IsZero() {
		t.Error("unmanaged namespace was deleted")
	}
}

func TestDeletionRemovesNamespaceAndFinalizer(t *testing.T) {
	requireEnvtest(t)
	name := "team-delete"
	createAndReconcile(t, newTN(name))

	if err := k8s.Delete(ctx, getTN(t, name)); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, k8s, name)

	if err := k8s.Get(ctx, types.NamespacedName{Name: name}, &idpv1alpha1.TeamNamespace{}); !apierrors.IsNotFound(err) {
		t.Errorf("TeamNamespace still present: %v", err)
	}
	// envtest runs no namespace controller, so the namespace stays Terminating.
	ns := &corev1.Namespace{}
	err := k8s.Get(ctx, types.NamespacedName{Name: name}, ns)
	if err == nil && ns.DeletionTimestamp.IsZero() {
		t.Error("managed namespace was not deleted")
	} else if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestDeletionSparesNamespaceThatLostItsLabels(t *testing.T) {
	requireEnvtest(t)
	name := "team-unlabelled"
	createAndReconcile(t, newTN(name))

	ns := get(t, &corev1.Namespace{}, "", name)
	delete(ns.Labels, LabelManagedBy)
	if err := k8s.Update(ctx, ns); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Delete(ctx, getTN(t, name)); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, k8s, name)

	if got := get(t, &corev1.Namespace{}, "", name); !got.DeletionTimestamp.IsZero() {
		t.Error("namespace without the managed-by label was deleted")
	}
}

func TestCELValidation(t *testing.T) {
	requireEnvtest(t)
	cases := []struct {
		name   string
		mutate func(*idpv1alpha1.TeamNamespace)
		want   string
	}{
		{"bad-email-1", func(tn *idpv1alpha1.TeamNamespace) { tn.Spec.Owner = "not-an-email" }, "owner must be an email"},
		{"bad-email-2", func(tn *idpv1alpha1.TeamNamespace) { tn.Spec.Owner = "a@b" }, "owner must be an email"},
		{"bad-email-3", func(tn *idpv1alpha1.TeamNamespace) { tn.Spec.Owner = "a b@example.com" }, "owner must be an email"},
		{"has.dot", nil, "DNS-1123 label"},
		{strings.Repeat("a", 41), nil, "at most 40"},
		{"kube-anything", nil, "reserved"},
		{"istio-gateways", nil, "reserved"},
		{"gke-managed", nil, "reserved"},
		{"default", nil, "reserved"},
		{"idp-system", nil, "reserved"},
		{"echo", nil, "reserved"},
		{"vault", nil, "reserved"},
		{"vault-secrets-operator", nil, "reserved"},
		{"bad-bu", func(tn *idpv1alpha1.TeamNamespace) { tn.Spec.BusinessUnit = "Payments_Team" }, "businessUnit"},
		{"bad-size", func(tn *idpv1alpha1.TeamNamespace) { tn.Spec.Size = "xl" }, "size"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tn := newTN(c.name)
			if c.mutate != nil {
				c.mutate(tn)
			}
			err := k8s.Create(ctx, tn)
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want Invalid containing %q, got %v", c.want, err)
			}
		})
	}

	ok := newTN(strings.Repeat("b", 40))
	if err := k8s.Create(ctx, ok); err != nil {
		t.Fatalf("40-char name should be accepted: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "samples", "idp_v1alpha1_teamnamespace.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sample := &idpv1alpha1.TeamNamespace{}
	if err := yaml.UnmarshalStrict(raw, sample); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(ctx, sample); err != nil {
		t.Fatalf("sample should be accepted: %v", err)
	}
}

func TestSpecImmutability(t *testing.T) {
	requireEnvtest(t)
	name := "team-immutable"
	if err := k8s.Create(ctx, newTN(name)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		want   string
		mutate func(*idpv1alpha1.TeamNamespace)
	}{
		{"businessUnit is immutable", func(tn *idpv1alpha1.TeamNamespace) { tn.Spec.BusinessUnit = "other" }},
		{"orderId is immutable", func(tn *idpv1alpha1.TeamNamespace) { tn.Spec.OrderID = "ORD-2026-999999" }},
		{"orderId is immutable", func(tn *idpv1alpha1.TeamNamespace) { tn.Spec.OrderID = "" }},
	} {
		tn := getTN(t, name)
		c.mutate(tn)
		if err := k8s.Update(ctx, tn); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want Invalid %q, got %v", c.want, err)
		}
	}
	tn := getTN(t, name)
	tn.Spec.Owner = "new-owner@example.com"
	if err := k8s.Update(ctx, tn); err != nil {
		t.Errorf("owner transfer should be allowed: %v", err)
	}
}

func TestManagerConvergesViaWatches(t *testing.T) {
	requireEnvtest(t)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&TeamNamespaceReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	mctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(mctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})

	name := "team-manager"
	if err := k8s.Create(ctx, newTN(name)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "TeamNamespace becomes Ready", func() error {
		if p := getTN(t, name).Status.Phase; p != idpv1alpha1.PhaseReady {
			return fmt.Errorf("phase %q", p)
		}
		return nil
	})

	if err := k8s.Delete(ctx, &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: SameNamespacePolicy, Namespace: name}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "deleted NetworkPolicy is recreated", func() error {
		return k8s.Get(ctx, types.NamespacedName{Namespace: name, Name: SameNamespacePolicy}, &networkingv1.NetworkPolicy{})
	})

	ns := get(t, &corev1.Namespace{}, "", name)
	ns.Labels["istio.io/dataplane-mode"] = "none"
	if err := k8s.Update(ctx, ns); err != nil {
		t.Fatal(err)
	}
	eventually(t, "namespace label drift is corrected", func() error {
		if v := get(t, &corev1.Namespace{}, "", name).Labels["istio.io/dataplane-mode"]; v != "ambient" {
			return errors.New(v)
		}
		return nil
	})
}
