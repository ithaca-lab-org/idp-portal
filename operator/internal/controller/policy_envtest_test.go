package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

// These tests drive the apiserver as the operator's ServiceAccount (via
// impersonation) with the generated RBAC from config/rbac and the admission
// policy from config/policy installed. Negative cases use server-side dry-run:
// RBAC and admission run in full, but nothing is persisted if a guard is missing.

const (
	operatorUser = "system:serviceaccount:idp-system:idp-operator"
	policyName   = "idp-operator-scope"
)

var (
	opOnce   sync.Once
	opClient client.Client
	opErr    error
)

func operatorClient(t *testing.T) client.Client {
	t.Helper()
	requireEnvtest(t)
	opOnce.Do(func() { opClient, opErr = setupOperatorIdentity() })
	if opErr != nil {
		t.Fatalf("operator identity setup: %v", opErr)
	}
	waitPolicy(t, opClient, true)
	return opClient
}

func setupOperatorIdentity() (client.Client, error) {
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "idp-system"}}); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, err
	}
	for _, f := range []string{
		filepath.Join("..", "..", "config", "rbac", "role.yaml"),
		filepath.Join("..", "..", "config", "policy", "operator-scope.yaml"),
		filepath.Join("..", "..", "config", "policy", "portal-scope.yaml"),
		filepath.Join("..", "..", "config", "policy", "tenant-services.yaml"),
	} {
		if err := applyFile(f); err != nil {
			return nil, err
		}
	}
	sa := rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "idp-operator", Namespace: "idp-system"}
	for _, o := range []client.Object{
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "idp-operator"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "idp-operator"},
			Subjects:   []rbacv1.Subject{sa},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "idp-operator", Namespace: "idp-system"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "idp-operator"},
			Subjects:   []rbacv1.Subject{sa},
		},
	} {
		if err := k8s.Create(ctx, o); err != nil {
			return nil, err
		}
	}

	c := rest.CopyConfig(cfg)
	c.Impersonate = rest.ImpersonationConfig{
		UserName: operatorUser,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:idp-system", "system:authenticated"},
	}
	return client.New(c, client.Options{Scheme: testScheme})
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
			return fmt.Errorf("%s: %w", path, err)
		}
		if len(u.Object) == 0 {
			continue
		}
		if err := k8s.Create(ctx, u); err != nil {
			return fmt.Errorf("%s %s: %w", u.GetKind(), u.GetName(), err)
		}
	}
}

// probeNamespace is an unlabelled namespace create: allowed by RBAC, denied only by the policy.
func probeNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "policy-probe"}}
}

// waitPolicy blocks until the policy is (or is not) enforced for the operator,
// since policy and binding reach the admission plugin through informers.
func waitPolicy(t *testing.T, op client.Client, active bool) {
	t.Helper()
	eventually(t, fmt.Sprintf("policy active=%v", active), func() error {
		err := op.Create(ctx, probeNamespace(), client.DryRunAll)
		if denied := isPolicyDenial(err); denied != active {
			return fmt.Errorf("probe create: %v", err)
		}
		return nil
	})
	if active {
		p := &admissionv1.ValidatingAdmissionPolicy{}
		if err := k8s.Get(ctx, types.NamespacedName{Name: policyName}, p); err != nil {
			t.Fatal(err)
		}
		if tc := p.Status.TypeChecking; tc != nil && len(tc.ExpressionWarnings) > 0 {
			t.Fatalf("policy type-check warnings: %+v", tc.ExpressionWarnings)
		}
	}
}

func isPolicyDenial(err error) bool {
	return apierrors.IsForbidden(err) && strings.Contains(err.Error(), policyName)
}

func wantPolicyDenial(t *testing.T, what string, err error) {
	t.Helper()
	if !isPolicyDenial(err) {
		t.Errorf("%s: want denial by %s, got %v", what, policyName, err)
	}
}

// wantRuleDenial requires a denial by this policy whose message names the
// specific rule, so a denial by another rule cannot satisfy the test.
func wantRuleDenial(t *testing.T, what, fragment string, err error) {
	t.Helper()
	if !isPolicyDenial(err) || !strings.Contains(err.Error(), fragment) {
		t.Errorf("%s: want denial by %s containing %q, got %v", what, policyName, fragment, err)
	}
}

func childMeta(name, ns string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{
		LabelManagedBy: ManagedByValue, LabelTeamNamespace: ns}}
}

func wantForbidden(t *testing.T, what string, err error) {
	t.Helper()
	if !apierrors.IsForbidden(err) {
		t.Errorf("%s: want Forbidden, got %v", what, err)
	}
}

func adminRB(ns string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: AdminRoleBindingName, Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: AdminClusterRole},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "attacker@example.com"}},
	}
}

func spinnakerRB(ns string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: SpinnakerDeployerBindingName, Namespace: ns},
		RoleRef:    spinnakerRoleRef(),
		Subjects:   spinnakerSubjects(),
	}
}

func clusterAdminRB(ns string) *rbacv1.RoleBinding {
	rb := adminRB(ns)
	rb.RoleRef.Name = "cluster-admin"
	return rb
}

func ensurePlainNamespace(t *testing.T, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"owner": "someone-else"}}}
	if err := k8s.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}

// TestOperatorLifecycleUnderRBACAndPolicy runs the real reconciler as the
// ServiceAccount: create (including the bind-only RoleBinding to admin), owner
// transfer, roleRef drift repair, and delete.
func TestOperatorLifecycleUnderRBACAndPolicy(t *testing.T) {
	op := operatorClient(t)
	name := "team-rbac"
	if err := k8s.Create(ctx, newTN(name)); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, op, name)
	assertStatus(t, getTN(t, name), idpv1alpha1.PhaseReady, ReasonReconciled)
	rb := get(t, &rbacv1.RoleBinding{}, name, AdminRoleBindingName)
	if rb.RoleRef.Name != AdminClusterRole || len(rb.Subjects) != 1 || rb.Subjects[0].Name != "dev@example.com" {
		t.Fatalf("rolebinding = %+v / %+v", rb.RoleRef, rb.Subjects)
	}

	// The operator holds bind on admin but not escalate: a binding to any other
	// ClusterRole is refused by the RBAC escalation check (and by the policy).
	wantForbidden(t, "operator binds cluster-admin", op.Create(ctx, func() *rbacv1.RoleBinding {
		b := clusterAdminRB(name)
		b.Name = "escalate"
		return b
	}(), client.DryRunAll))

	tn := getTN(t, name)
	tn.Spec.Owner = "new-owner@example.com"
	if err := k8s.Update(ctx, tn); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, op, name)
	if rb := get(t, &rbacv1.RoleBinding{}, name, AdminRoleBindingName); rb.Subjects[0].Name != "new-owner@example.com" {
		t.Errorf("owner transfer not applied: %+v", rb.Subjects)
	}

	// An admin swaps the binding for one with a foreign roleRef; the operator
	// must be able to delete and recreate it.
	if err := k8s.Delete(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: AdminRoleBindingName, Namespace: name}}); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(ctx, clusterAdminRB(name)); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, op, name)
	reconcileOnce(t, op, name)
	if rb := get(t, &rbacv1.RoleBinding{}, name, AdminRoleBindingName); rb.RoleRef.Name != AdminClusterRole {
		t.Errorf("roleRef not repaired: %+v", rb.RoleRef)
	}
	assertStatus(t, getTN(t, name), idpv1alpha1.PhaseReady, ReasonReconciled)

	if err := k8s.Delete(ctx, getTN(t, name)); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, op, name)
	if err := k8s.Get(ctx, types.NamespacedName{Name: name}, &idpv1alpha1.TeamNamespace{}); !apierrors.IsNotFound(err) {
		t.Errorf("TeamNamespace still present: %v", err)
	}
	ns := &corev1.Namespace{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: name}, ns); err == nil && ns.DeletionTimestamp.IsZero() {
		t.Error("managed namespace was not deleted by the operator")
	}
}

func TestPolicyDeniesOperatorOutsideManagedNamespaces(t *testing.T) {
	op := operatorClient(t)
	managed := "team-guarded"
	if err := k8s.Create(ctx, newTN(managed)); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, op, managed)
	assertStatus(t, getTN(t, managed), idpv1alpha1.PhaseReady, ReasonReconciled)
	plain := "plain-ns"
	ensurePlainNamespace(t, plain)
	dry := client.DryRunAll

	t.Run("namespace create", func(t *testing.T) {
		wantPolicyDenial(t, "create unlabelled", op.Create(ctx, probeNamespace(), dry))
		wrong := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-x", Labels: map[string]string{
			LabelManagedBy: ManagedByValue, LabelTeamNamespace: "team-y", "pod-security.kubernetes.io/enforce": "baseline",
		}}}
		wantPolicyDenial(t, "create with mismatched team label", op.Create(ctx, wrong, dry))
		reserved := []string{"kube-evil", "istio-evil", "gke-evil", "vault-evil"}
		for n := range reservedNames {
			reserved = append(reserved, n)
		}
		for _, n := range reserved {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n, Labels: map[string]string{
				LabelManagedBy: ManagedByValue, LabelTeamNamespace: n, "pod-security.kubernetes.io/enforce": "baseline",
			}}}
			err := op.Create(ctx, ns, dry)
			if !isPolicyDenial(err) && !apierrors.IsAlreadyExists(err) {
				t.Errorf("create reserved %s: want policy denial, got %v", n, err)
			}
		}
	})

	t.Run("namespace delete", func(t *testing.T) {
		// kube-system, default, kube-public and kube-node-lease are also immortal via
		// the built-in NamespaceLifecycle plugin, which answers first.
		wantForbidden(t, "delete kube-system", op.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}}, dry))
		wantPolicyDenial(t, "delete unlabelled", op.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: plain}}, dry))
		wantForbidden(t, "deletecollection namespaces", op.DeleteAllOf(ctx, &corev1.Namespace{}, dry))
	})

	t.Run("namespace update", func(t *testing.T) {
		adopt := get(t, &corev1.Namespace{}, "", plain)
		adopt.Labels[LabelManagedBy] = ManagedByValue
		adopt.Labels[LabelTeamNamespace] = plain
		adopt.Labels["pod-security.kubernetes.io/enforce"] = "baseline"
		wantPolicyDenial(t, "label unmanaged namespace", op.Update(ctx, adopt, dry))

		ks := get(t, &corev1.Namespace{}, "", "kube-system")
		ks.Labels["x"] = "y"
		wantPolicyDenial(t, "update kube-system", op.Update(ctx, ks, dry))

		for what, mutate := range map[string]func(*corev1.Namespace){
			"drop managed-by":       func(n *corev1.Namespace) { delete(n.Labels, LabelManagedBy) },
			"change team label":     func(n *corev1.Namespace) { n.Labels[LabelTeamNamespace] = "other" },
			"privileged pod policy": func(n *corev1.Namespace) { n.Labels["pod-security.kubernetes.io/enforce"] = "privileged" },
		} {
			ns := get(t, &corev1.Namespace{}, "", managed)
			mutate(ns)
			wantPolicyDenial(t, what, op.Update(ctx, ns, dry))
		}
		ns := get(t, &corev1.Namespace{}, "", managed)
		if err := op.Update(ctx, ns, dry); err != nil {
			t.Errorf("update of managed namespace with its exact labels: %v", err)
		}
		const nsRule = "only the label keys the controller writes"
		for what, mutate := range map[string]func(*corev1.Namespace){
			"ambient removed":      func(n *corev1.Namespace) { delete(n.Labels, LabelDataplaneMode) },
			"ambient set to none":  func(n *corev1.Namespace) { n.Labels[LabelDataplaneMode] = "none" },
			"use-waypoint added":   func(n *corev1.Namespace) { n.Labels["istio.io/use-waypoint"] = "waypoint" },
			"istio-injection":      func(n *corev1.Namespace) { n.Labels["istio-injection"] = "enabled" },
			"istio.io/rev":         func(n *corev1.Namespace) { n.Labels["istio.io/rev"] = "x" },
			"gateway-access gone":  func(n *corev1.Namespace) { delete(n.Labels, LabelGatewayAccess) },
			"gateway-access other": func(n *corev1.Namespace) { n.Labels[LabelGatewayAccess] = "denied" },
			"arbitrary label":      func(n *corev1.Namespace) { n.Labels["team/extra"] = "ok" },
		} {
			ns := get(t, &corev1.Namespace{}, "", managed)
			mutate(ns)
			wantRuleDenial(t, "update namespace: "+what, nsRule, op.Update(ctx, ns, dry))
		}
		// The same label rules apply on create (the rest of the labels are valid).
		for what, mutate := range map[string]func(map[string]string){
			"no ambient":   func(l map[string]string) { delete(l, LabelDataplaneMode) },
			"use-waypoint": func(l map[string]string) { l["istio.io/use-waypoint"] = "waypoint" },
			"no gateway":   func(l map[string]string) { delete(l, LabelGatewayAccess) },
			"arbitrary":    func(l map[string]string) { l["team/extra"] = "ok" },
		} {
			l := namespaceLabels(newTN("team-labels"))
			mutate(l)
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-labels", Labels: l}}
			wantRuleDenial(t, "create namespace: "+what, nsRule, op.Create(ctx, ns, dry))
		}
		okNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-labels", Labels: namespaceLabels(newTN("team-labels"))}}
		if err := op.Create(ctx, okNS, dry); err != nil {
			t.Errorf("create namespace with the exact controller labels: %v", err)
		}
	})

	t.Run("rolebindings", func(t *testing.T) {
		wantPolicyDenial(t, "admin RB in kube-system", op.Create(ctx, adminRB("kube-system"), dry))
		wantPolicyDenial(t, "admin RB in unlabelled ns", op.Create(ctx, adminRB(plain), dry))

		subjects := map[string][]rbacv1.Subject{
			"group":               {{APIGroup: rbacv1.GroupName, Kind: rbacv1.GroupKind, Name: "system:authenticated"}},
			"serviceaccount":      {{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: managed}},
			"sa as user":          {{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "system:serviceaccount:kube-system:default"}},
			"anonymous user":      {{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "system:anonymous"}},
			"wildcard user":       {{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "*"}},
			"two users":           {{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "a@example.com"}, {APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "b@example.com"}},
			"no subjects (empty)": nil,
		}
		for what, s := range subjects {
			rb := get(t, &rbacv1.RoleBinding{}, managed, AdminRoleBindingName)
			rb.Subjects = s
			wantPolicyDenial(t, "update RB with "+what, op.Update(ctx, rb, dry))
		}

		wantForbidden(t, "cluster-admin RB in managed ns", op.Create(ctx, func() *rbacv1.RoleBinding {
			b := clusterAdminRB(managed)
			b.Name = "escalate"
			return b
		}(), dry))
		other := adminRB(managed)
		other.Name = "second-admin"
		wantPolicyDenial(t, "admin RB with a foreign name", op.Create(ctx, other, dry))
		// The Spinnaker deployer binding: exactly one roleRef and subject.
		if err := op.Create(ctx, spinnakerRB(managed), dry); err != nil {
			t.Errorf("exact spinnaker-deployer binding: %v", err)
		}
		wantRuleDenial(t, "spinnaker RB in kube-system", "may only write rolebindings", op.Create(ctx, spinnakerRB("kube-system"), dry))
		wantRuleDenial(t, "spinnaker RB in unlabelled ns", "may only write rolebindings", op.Create(ctx, spinnakerRB(plain), dry))
		for what, mut := range map[string]func(*rbacv1.RoleBinding){
			"roleRef admin":        func(b *rbacv1.RoleBinding) { b.RoleRef.Name = AdminClusterRole },
			"other serviceaccount": func(b *rbacv1.RoleBinding) { b.Subjects[0].Name = "orca" },
			"other sa namespace":   func(b *rbacv1.RoleBinding) { b.Subjects[0].Namespace = managed },
			"user subject": func(b *rbacv1.RoleBinding) {
				b.Subjects = []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "a@example.com"}}
			},
			"group subject": func(b *rbacv1.RoleBinding) {
				b.Subjects = []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.GroupKind, Name: "system:authenticated"}}
			},
			"extra subject": func(b *rbacv1.RoleBinding) {
				b.Subjects = append(b.Subjects, rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: managed})
			},
			"no subjects": func(b *rbacv1.RoleBinding) { b.Subjects = nil },
		} {
			b := spinnakerRB(managed)
			mut(b)
			wantRuleDenial(t, "spinnaker RB with "+what, "exactly ServiceAccount spinnaker/clouddriver", op.Create(ctx, b, dry))
		}
		// The admin name cannot carry the Spinnaker grant, nor the reverse.
		swap := adminRB(managed)
		swap.RoleRef.Name, swap.Subjects = SpinnakerDeployerClusterRole, spinnakerSubjects()
		wantRuleDenial(t, "idp-namespace-admin to the deployer role", "must bind ClusterRole admin", op.Create(ctx, swap, dry))
		// UPDATE of an existing deployer binding may not add a second subject.
		if err := k8s.Create(ctx, spinnakerRB(managed)); err != nil {
			t.Fatal(err)
		}
		cur := get(t, &rbacv1.RoleBinding{}, managed, SpinnakerDeployerBindingName)
		cur.Subjects = append(cur.Subjects, rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: managed})
		wantRuleDenial(t, "update spinnaker RB adding a subject", "exactly ServiceAccount spinnaker/clouddriver", op.Update(ctx, cur, dry))
		if err := k8s.Delete(ctx, cur); err != nil {
			t.Fatal(err)
		}
		wantForbidden(t, "deletecollection rolebindings", op.DeleteAllOf(ctx, &rbacv1.RoleBinding{}, client.InNamespace(managed), dry))
	})

	t.Run("other children", func(t *testing.T) {
		q := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: QuotaName, Namespace: "kube-system"}}
		wantPolicyDenial(t, "quota in kube-system", op.Create(ctx, q, dry))
		lr := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: LimitRangeName, Namespace: plain},
			Spec: desiredLimitRangeSpec(newTN(plain))}
		wantPolicyDenial(t, "limitrange in unlabelled ns", op.Create(ctx, lr, dry))
		allow := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "allow-all", Namespace: managed},
			Spec: networkingv1.NetworkPolicySpec{Ingress: []networkingv1.NetworkPolicyIngressRule{{}}}}
		wantPolicyDenial(t, "extra networkpolicy in managed ns", op.Create(ctx, allow, dry))
	})

	t.Run("gateway networkpolicy", func(t *testing.T) {
		mk := func(name string, mut func(*networkingv1.NetworkPolicySpec)) *networkingv1.NetworkPolicy {
			np := &networkingv1.NetworkPolicy{ObjectMeta: childMeta(name, managed),
				Spec: desiredAllowIngressGatewaySpec(GatewayOptions{})}
			if mut != nil {
				mut(&np.Spec)
			}
			return np
		}
		// Allowed: the exact spec, on update too (the policy already exists).
		if err := op.Create(ctx, mk("idp-allow-ingress-gateway-x", nil), dry); !isPolicyDenial(err) {
			t.Errorf("a different name must be denied, got %v", err)
		}
		existing := get(t, &networkingv1.NetworkPolicy{}, managed, GatewayPolicyName)
		existing.Spec = desiredAllowIngressGatewaySpec(GatewayOptions{})
		if err := op.Update(ctx, existing, dry); err != nil {
			t.Errorf("exact spec update denied: %v", err)
		}
		gone := mk(GatewayPolicyName, nil)
		gone.Namespace = plain
		wantPolicyDenial(t, "gateway policy in unmanaged ns", op.Create(ctx, gone, dry))

		sel := func(k, v string) *metav1.LabelSelector {
			return &metav1.LabelSelector{MatchLabels: map[string]string{k: v}}
		}
		// An entirely empty peer, and an ipBlock combined with selectors, are
		// rejected by the built-in NetworkPolicy validation before admission.
		cases := map[string]func(*networkingv1.NetworkPolicySpec){
			"extra peer": func(s *networkingv1.NetworkPolicySpec) {
				s.Ingress[0].From = append(s.Ingress[0].From, networkingv1.NetworkPolicyPeer{PodSelector: sel("a", "b")})
			},
			"empty selectors": func(s *networkingv1.NetworkPolicySpec) {
				s.Ingress[0].From = []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{}, PodSelector: &metav1.LabelSelector{}}}
			},
			"no from (allow all)": func(s *networkingv1.NetworkPolicySpec) { s.Ingress[0].From = nil },
			"empty rule":          func(s *networkingv1.NetworkPolicySpec) { s.Ingress = []networkingv1.NetworkPolicyIngressRule{{}} },
			"ipBlock": func(s *networkingv1.NetworkPolicySpec) {
				s.Ingress[0].From = []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}}
			},
			"namespace only": func(s *networkingv1.NetworkPolicySpec) { s.Ingress[0].From[0].PodSelector = nil },
			"pods only":      func(s *networkingv1.NetworkPolicySpec) { s.Ingress[0].From[0].NamespaceSelector = nil },
			"wrong namespace": func(s *networkingv1.NetworkPolicySpec) {
				s.Ingress[0].From[0].NamespaceSelector = sel("kubernetes.io/metadata.name", "kube-system")
			},
			"wrong pod selector": func(s *networkingv1.NetworkPolicySpec) {
				s.Ingress[0].From[0].PodSelector = sel("gateway.networking.k8s.io/gateway-name", "other")
			},
			"extra pod label": func(s *networkingv1.NetworkPolicySpec) {
				s.Ingress[0].From[0].PodSelector.MatchLabels["x"] = "y"
			},
			"matchExpressions": func(s *networkingv1.NetworkPolicySpec) {
				s.Ingress[0].From[0].PodSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "gateway.networking.k8s.io/gateway-name", Operator: metav1.LabelSelectorOpExists}}}
			},
			"no ports": func(s *networkingv1.NetworkPolicySpec) { s.Ingress[0].Ports = nil },
			"port 8080": func(s *networkingv1.NetworkPolicySpec) {
				p := intstr.FromInt32(8080)
				s.Ingress[0].Ports[0].Port = &p
			},
			"second port": func(s *networkingv1.NetworkPolicySpec) {
				p := intstr.FromInt32(8080)
				s.Ingress[0].Ports = append(s.Ingress[0].Ports, networkingv1.NetworkPolicyPort{Port: &p})
			},
			"UDP": func(s *networkingv1.NetworkPolicySpec) {
				u := corev1.ProtocolUDP
				s.Ingress[0].Ports[0].Protocol = &u
			},
			"port range": func(s *networkingv1.NetworkPolicySpec) {
				e := int32(16000)
				s.Ingress[0].Ports[0].EndPort = &e
			},
			"second rule": func(s *networkingv1.NetworkPolicySpec) {
				s.Ingress = append(s.Ingress, networkingv1.NetworkPolicyIngressRule{})
			},
			"narrowed target pods": func(s *networkingv1.NetworkPolicySpec) { s.PodSelector = *sel("a", "b") },
			"egress type": func(s *networkingv1.NetworkPolicySpec) {
				s.PolicyTypes = append(s.PolicyTypes, networkingv1.PolicyTypeEgress)
			},
		}
		for what, mut := range cases {
			wantRuleDenial(t, "create gateway policy with "+what, "idp-allow-ingress-gateway only as", op.Create(ctx, mk("idp-allow-ingress-gateway", mut), dry))
			bad := get(t, &networkingv1.NetworkPolicy{}, managed, GatewayPolicyName)
			bad.Spec = mk(GatewayPolicyName, mut).Spec
			wantRuleDenial(t, "update gateway policy with "+what, "idp-allow-ingress-gateway only as", op.Update(ctx, bad, dry))
		}
	})

	// The two other managed policies are pinned to their exact specs too.
	// Shapes the built-in NetworkPolicy validation rejects before admission
	// (a peer with no field set, an ipBlock combined with selectors) never reach
	// the policy, so they are not listed; the shapes below pass validation.
	t.Run("deny-ingress and same-namespace networkpolicy", func(t *testing.T) {
		sel := func(k, v string) *metav1.LabelSelector {
			return &metav1.LabelSelector{MatchLabels: map[string]string{k: v}}
		}
		type pin struct {
			name  string
			spec  func() networkingv1.NetworkPolicySpec
			cases map[string]func(*networkingv1.NetworkPolicySpec)
		}
		common := map[string]func(*networkingv1.NetworkPolicySpec){
			"egress type": func(s *networkingv1.NetworkPolicySpec) {
				s.PolicyTypes = append(s.PolicyTypes, networkingv1.PolicyTypeEgress)
			},
			"egress only": func(s *networkingv1.NetworkPolicySpec) {
				s.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}
			},
			// Empty policyTypes is not listed: the API server defaults it to [Ingress]
			// before admission, which is the pinned value.
			"narrowed target pods": func(s *networkingv1.NetworkPolicySpec) { s.PodSelector = *sel("a", "b") },
			"matchExpressions target pods": func(s *networkingv1.NetworkPolicySpec) {
				s.PodSelector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "a", Operator: metav1.LabelSelectorOpExists}}}
			},
			"egress rule": func(s *networkingv1.NetworkPolicySpec) {
				s.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
			},
			"empty ingress rule (allow all)": func(s *networkingv1.NetworkPolicySpec) {
				s.Ingress = []networkingv1.NetworkPolicyIngressRule{{}}
			},
			"ipBlock 0.0.0.0/0": func(s *networkingv1.NetworkPolicySpec) {
				s.Ingress = []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}}}}
			},
			"empty namespaceSelector peer": func(s *networkingv1.NetworkPolicySpec) {
				s.Ingress = []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}}}}
			},
			"empty ns and pod selectors": func(s *networkingv1.NetworkPolicySpec) {
				s.Ingress = []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{}, PodSelector: &metav1.LabelSelector{}}}}}
			},
			"port list only": func(s *networkingv1.NetworkPolicySpec) {
				p := intstr.FromInt32(8080)
				s.Ingress = []networkingv1.NetworkPolicyIngressRule{{
					Ports: []networkingv1.NetworkPolicyPort{{Port: &p}}}}
			},
		}
		pins := []pin{
			{DenyIngressPolicyName, desiredDenyIngressSpec, map[string]func(*networkingv1.NetworkPolicySpec){
				"same-namespace rule": func(s *networkingv1.NetworkPolicySpec) {
					*s = desiredSameNamespaceSpec()
				},
			}},
			{SameNamespacePolicy, desiredSameNamespaceSpec, map[string]func(*networkingv1.NetworkPolicySpec){
				"no ingress rule": func(s *networkingv1.NetworkPolicySpec) { s.Ingress = nil },
				"extra peer": func(s *networkingv1.NetworkPolicySpec) {
					s.Ingress[0].From = append(s.Ingress[0].From, networkingv1.NetworkPolicyPeer{PodSelector: sel("a", "b")})
				},
				"no from (allow all)": func(s *networkingv1.NetworkPolicySpec) { s.Ingress[0].From = nil },
				"ipBlock peer": func(s *networkingv1.NetworkPolicySpec) {
					s.Ingress[0].From = []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}}
				},
				"namespaceSelector added": func(s *networkingv1.NetworkPolicySpec) {
					s.Ingress[0].From[0].NamespaceSelector = &metav1.LabelSelector{}
				},
				"namespace peer only": func(s *networkingv1.NetworkPolicySpec) {
					s.Ingress[0].From = []networkingv1.NetworkPolicyPeer{{NamespaceSelector: sel("kubernetes.io/metadata.name", "kube-system")}}
				},
				"narrowed peer pods": func(s *networkingv1.NetworkPolicySpec) {
					s.Ingress[0].From[0].PodSelector = sel("a", "b")
				},
				"matchExpressions peer pods": func(s *networkingv1.NetworkPolicySpec) {
					s.Ingress[0].From[0].PodSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
						{Key: "a", Operator: metav1.LabelSelectorOpExists}}}
				},
				"ports added": func(s *networkingv1.NetworkPolicySpec) {
					p := intstr.FromInt32(8080)
					s.Ingress[0].Ports = []networkingv1.NetworkPolicyPort{{Port: &p}}
				},
				"second rule": func(s *networkingv1.NetworkPolicySpec) {
					s.Ingress = append(s.Ingress, desiredSameNamespaceSpec().Ingress[0])
				},
			}},
		}
		for _, p := range pins {
			existing := get(t, &networkingv1.NetworkPolicy{}, managed, p.name)
			existing.Spec = p.spec()
			if err := op.Update(ctx, existing, dry); err != nil {
				t.Errorf("exact %s spec update denied: %v", p.name, err)
			}
			okCreate := &networkingv1.NetworkPolicy{ObjectMeta: childMeta(p.name, managed), Spec: p.spec()}
			// Already exists: AlreadyExists, not a policy denial, proves the spec passed.
			if err := op.Create(ctx, okCreate, dry); isPolicyDenial(err) {
				t.Errorf("exact %s spec create denied: %v", p.name, err)
			}
			all := map[string]func(*networkingv1.NetworkPolicySpec){}
			for k, v := range common {
				all[k] = v
			}
			for k, v := range p.cases {
				all[k] = v
			}
			for what, mut := range all {
				spec := p.spec()
				mut(&spec)
				create := &networkingv1.NetworkPolicy{ObjectMeta: childMeta(p.name, managed), Spec: spec}
				wantRuleDenial(t, "create "+p.name+" with "+what, p.name+" only as", op.Create(ctx, create, dry))
				bad := get(t, &networkingv1.NetworkPolicy{}, managed, p.name)
				bad.Spec = spec
				wantRuleDenial(t, "update "+p.name+" with "+what, p.name+" only as", op.Update(ctx, bad, dry))
			}
		}
	})

	t.Run("managed networkpolicy metadata", func(t *testing.T) {
		const metaRule = "NetworkPolicy metadata"
		for _, name := range []string{DenyIngressPolicyName, SameNamespacePolicy, GatewayPolicyName} {
			spec := desiredDenyIngressSpec()
			switch name {
			case SameNamespacePolicy:
				spec = desiredSameNamespaceSpec()
			case GatewayPolicyName:
				spec = desiredAllowIngressGatewaySpec(GatewayOptions{})
			}
			for what, mut := range map[string]func(*metav1.ObjectMeta){
				"no labels":        func(m *metav1.ObjectMeta) { m.Labels = nil },
				"extra label":      func(m *metav1.ObjectMeta) { m.Labels["x"] = "y" },
				"annotation":       func(m *metav1.ObjectMeta) { m.Annotations = map[string]string{"x": "y"} },
				"wrong team label": func(m *metav1.ObjectMeta) { m.Labels[LabelTeamNamespace] = "other" },
			} {
				m := childMeta(name, managed)
				mut(&m)
				wantRuleDenial(t, "create "+name+" with "+what, metaRule, op.Create(ctx, &networkingv1.NetworkPolicy{ObjectMeta: m, Spec: spec}, dry))
			}
			// Update: an added label or annotation is denied; unchanged metadata passes.
			cur := get(t, &networkingv1.NetworkPolicy{}, managed, name)
			if err := op.Update(ctx, cur, dry); err != nil {
				t.Errorf("update %s with unchanged metadata: %v", name, err)
			}
			cur = get(t, &networkingv1.NetworkPolicy{}, managed, name)
			cur.Labels["x"] = "y"
			wantRuleDenial(t, "update "+name+" adding a label", metaRule, op.Update(ctx, cur, dry))
			cur = get(t, &networkingv1.NetworkPolicy{}, managed, name)
			cur.Annotations = map[string]string{"x": "y"}
			wantRuleDenial(t, "update "+name+" adding an annotation", metaRule, op.Update(ctx, cur, dry))
		}
	})
}

// TestPolicyIsWhatDenies removes the binding and shows that RBAC alone lets the
// operator do what the policy tests above expect to be denied; only the
// RBAC escalation check still stops a cluster-admin binding.
func TestPolicyIsWhatDenies(t *testing.T) {
	op := operatorClient(t)
	binding := &admissionv1.ValidatingAdmissionPolicyBinding{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: policyName}, binding); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Delete(ctx, binding); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		restored := &admissionv1.ValidatingAdmissionPolicyBinding{
			ObjectMeta: metav1.ObjectMeta{Name: policyName},
			Spec:       *binding.Spec.DeepCopy(),
		}
		if err := k8s.Create(ctx, restored); err != nil {
			t.Fatal(err)
		}
		waitPolicy(t, op, true)
	})
	waitPolicy(t, op, false)

	plain := "plain-ns"
	ensurePlainNamespace(t, plain)
	dry := client.DryRunAll
	if err := op.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: plain}}, dry); err != nil {
		t.Errorf("without the policy, RBAC should allow deleting an unlabelled namespace (dry-run): %v", err)
	}
	if err := op.Create(ctx, adminRB("kube-system"), dry); err != nil {
		t.Errorf("without the policy, RBAC should allow an admin RoleBinding in kube-system (dry-run): %v", err)
	}
	adopt := get(t, &corev1.Namespace{}, "", plain)
	adopt.Labels[LabelManagedBy] = ManagedByValue
	adopt.Labels[LabelTeamNamespace] = plain
	if err := op.Update(ctx, adopt, dry); err != nil {
		t.Errorf("without the policy, RBAC should allow labelling a foreign namespace (dry-run): %v", err)
	}
	if err := op.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: VaultReaderServiceAccount, Namespace: "kube-system"}}, dry); err != nil {
		t.Errorf("without the policy, RBAC should allow ServiceAccount vault-reader in kube-system (dry-run): %v", err)
	}
	if err := op.Create(ctx, vaultAuthFor(plain, "anything"), dry); err != nil {
		t.Errorf("without the policy, RBAC should allow any VaultAuth in an unlabelled namespace (dry-run): %v", err)
	}
	evil := vssFor(plain, "anything")
	unstructured.RemoveNestedField(evil.Object, "metadata", "ownerReferences")
	_ = unstructured.SetNestedField(evil.Object, "kube-system/x", "spec", "path")
	_ = unstructured.SetNestedField(evil.Object, true, "spec", "destination", "overwrite")
	if err := op.Create(ctx, evil, dry); err != nil {
		t.Errorf("without the policy, RBAC should allow any VaultStaticSecret in an unlabelled namespace (dry-run): %v", err)
	}
	err := op.Create(ctx, clusterAdminRB(plain), dry)
	if !apierrors.IsForbidden(err) || isPolicyDenial(err) {
		t.Errorf("cluster-admin binding must still be refused by RBAC alone, got %v", err)
	}
}

type purgerFunc func(ctx context.Context, ns string, sub ...string) error

func (f purgerFunc) Purge(ctx context.Context, ns string, sub ...string) error {
	return f(ctx, ns, sub...)
}

func vaultAuthFor(ns, name string) *unstructured.Unstructured {
	va := newVaultAuth(ns)
	va.SetName(name)
	va.Object["spec"] = desiredVaultAuthSpec()
	return va
}

// TestOperatorVaultAccessUnderRBACAndPolicy runs the reconciler with Vault
// enabled as the ServiceAccount, then checks that the policy confines the
// ServiceAccount and VaultAuth writes to the fixed names in managed namespaces.
func TestOperatorVaultAccessUnderRBACAndPolicy(t *testing.T) {
	op := operatorClient(t)
	var purged []string
	o := &VaultOptions{AuthMount: defaultVaultAuthMount, GiveUpAfter: time.Minute,
		Purger: purgerFunc(func(_ context.Context, ns string, _ ...string) error { purged = append(purged, ns); return nil })}
	managed := "team-vault-rbac"
	if err := k8s.Create(ctx, newTN(managed)); err != nil {
		t.Fatal(err)
	}
	reconcileVault(t, op, o, managed, &syncBuffer{})
	assertStatus(t, getTN(t, managed), idpv1alpha1.PhaseReady, ReasonReconciled)
	get(t, &corev1.ServiceAccount{}, managed, VaultReaderServiceAccount)
	getVaultAuth(t, managed)

	plain := "plain-ns"
	ensurePlainNamespace(t, plain)
	dry := client.DryRunAll
	sa := func(ns, name string) *corev1.ServiceAccount {
		return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	}

	t.Run("serviceaccounts", func(t *testing.T) {
		wantPolicyDenial(t, "vault-reader in kube-system", op.Create(ctx, sa("kube-system", VaultReaderServiceAccount), dry))
		wantPolicyDenial(t, "vault-reader in unlabelled ns", op.Create(ctx, sa(plain, VaultReaderServiceAccount), dry))
		wantPolicyDenial(t, "other SA in managed ns", op.Create(ctx, sa(managed, "builder"), dry))
		// envtest runs no ServiceAccount controller, so create "default" as admin.
		// RBAC pins update to vault-reader, so this one is refused before admission.
		if err := k8s.Create(ctx, sa(managed, "default")); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
		d := get(t, &corev1.ServiceAccount{}, managed, "default")
		d.AutomountServiceAccountToken = ptr.To(true)
		wantForbidden(t, "update default SA in managed ns", op.Update(ctx, d, dry))
		wantForbidden(t, "TokenRequest for vault-reader", op.SubResource("token").Create(ctx,
			sa(managed, VaultReaderServiceAccount), &authenticationv1.TokenRequest{
				Spec: authenticationv1.TokenRequestSpec{Audiences: []string{"vault"}},
			}))
		wantForbidden(t, "deletecollection serviceaccounts", op.DeleteAllOf(ctx, &corev1.ServiceAccount{}, client.InNamespace(managed), dry))
	})

	t.Run("vaultauths", func(t *testing.T) {
		wantPolicyDenial(t, "VaultAuth in kube-system", op.Create(ctx, vaultAuthFor("kube-system", VaultAuthName), dry))
		wantPolicyDenial(t, "VaultAuth in unlabelled ns", op.Create(ctx, vaultAuthFor(plain, VaultAuthName), dry))
		wantPolicyDenial(t, "VaultAuth with a foreign name", op.Create(ctx, vaultAuthFor(managed, "second"), dry))
		for what, mutate := range map[string]func(*unstructured.Unstructured){
			"purge role": func(u *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(u.Object, "idp-operator-purge", "spec", "kubernetes", "role")
			},
			"other SA": func(u *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(u.Object, "default", "spec", "kubernetes", "serviceAccount")
			},
			"jwt method":     func(u *unstructured.Unstructured) { _ = unstructured.SetNestedField(u.Object, "jwt", "spec", "method") },
			"no kubernetes":  func(u *unstructured.Unstructured) { unstructured.RemoveNestedField(u.Object, "spec", "kubernetes") },
			"no spec at all": func(u *unstructured.Unstructured) { delete(u.Object, "spec") },
			"other connection": func(u *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(u.Object, "evil/conn", "spec", "vaultConnectionRef")
			},
			"other mount": func(u *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(u.Object, "other", "spec", "mount")
			},
			"vault namespace": func(u *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(u.Object, "admin", "spec", "namespace")
			},
			"other audience": func(u *unstructured.Unstructured) {
				_ = unstructured.SetNestedStringSlice(u.Object, []string{"vault", "https://kubernetes.default.svc"}, "spec", "kubernetes", "audiences")
			},
			"long tokens": func(u *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(u.Object, int64(86400), "spec", "kubernetes", "tokenExpirationSeconds")
			},
			"headers": func(u *unstructured.Unstructured) {
				_ = unstructured.SetNestedStringMap(u.Object, map[string]string{"X-Vault-Namespace": "root"}, "spec", "headers")
			},
			"params": func(u *unstructured.Unstructured) {
				_ = unstructured.SetNestedStringMap(u.Object, map[string]string{"role": "x"}, "spec", "params")
			},
			"allowedNamespaces": func(u *unstructured.Unstructured) {
				_ = unstructured.SetNestedStringSlice(u.Object, []string{"*"}, "spec", "allowedNamespaces")
			},
			"vaultAuthGlobalRef": func(u *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(u.Object, map[string]any{"name": "g"}, "spec", "vaultAuthGlobalRef")
			},
			"jwt block": func(u *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(u.Object, map[string]any{"role": "x"}, "spec", "jwt")
			},
		} {
			va := getVaultAuth(t, managed)
			mutate(va)
			wantPolicyDenial(t, "update VaultAuth: "+what, op.Update(ctx, va, dry))
		}
		wantForbidden(t, "deletecollection vaultauths", op.DeleteAllOf(ctx, newVaultAuth(""), client.InNamespace(managed), dry))
	})

	if err := k8s.Delete(ctx, getTN(t, managed)); err != nil {
		t.Fatal(err)
	}
	reconcileVault(t, op, o, managed, &syncBuffer{})
	if err := k8s.Get(ctx, types.NamespacedName{Name: managed}, &idpv1alpha1.TeamNamespace{}); !apierrors.IsNotFound(err) {
		t.Errorf("TeamNamespace still present: %v", err)
	}
	// Once before access was granted, once on deletion.
	if !slices.Equal(purged, []string{managed, managed}) {
		t.Errorf("purged = %v", purged)
	}
}

// TestSpinnakerDeployerBinding runs the real reconciler as the operator
// ServiceAccount: the binding is absent while the flag is off, created with the
// exact roleRef and subject when on, repaired on subject and roleRef drift, and
// removed again when the flag goes off.
func TestSpinnakerDeployerBinding(t *testing.T) {
	op := operatorClient(t)
	name := "team-spinnaker"
	if err := k8s.Create(ctx, newTN(name)); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: name, Name: SpinnakerDeployerBindingName}
	absent := func(what string) {
		t.Helper()
		if err := k8s.Get(ctx, key, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
			t.Errorf("%s: spinnaker-deployer binding present or error: %v", what, err)
		}
	}

	reconcileOnceWith(t, op, name, false)
	absent("flag off")

	reconcileOnceWith(t, op, name, true)
	rb := get(t, &rbacv1.RoleBinding{}, name, SpinnakerDeployerBindingName)
	if rb.RoleRef != spinnakerRoleRef() || !equality.Semantic.DeepEqual(rb.Subjects, spinnakerSubjects()) {
		t.Fatalf("binding = %+v / %+v", rb.RoleRef, rb.Subjects)
	}
	if !metav1.IsControlledBy(rb, getTN(t, name)) {
		t.Errorf("binding is not controlled by the TeamNamespace: %+v", rb.OwnerReferences)
	}
	if get(t, &rbacv1.RoleBinding{}, name, AdminRoleBindingName).RoleRef.Name != AdminClusterRole {
		t.Error("admin binding changed")
	}
	rv := rb.ResourceVersion
	reconcileOnceWith(t, op, name, true)
	if got := get(t, &rbacv1.RoleBinding{}, name, SpinnakerDeployerBindingName).ResourceVersion; got != rv {
		t.Error("second reconcile rewrote the binding")
	}

	// Subject drift is corrected in place.
	rb.Subjects = append(rb.Subjects, rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: name})
	if err := k8s.Update(ctx, rb); err != nil {
		t.Fatal(err)
	}
	reconcileOnceWith(t, op, name, true)
	if rb := get(t, &rbacv1.RoleBinding{}, name, SpinnakerDeployerBindingName); !equality.Semantic.DeepEqual(rb.Subjects, spinnakerSubjects()) {
		t.Errorf("subject drift not corrected: %+v", rb.Subjects)
	}

	// A binding of that name the TeamNamespace does not control is left alone
	// (not repaired, deleted or overwritten), whatever its roleRef.
	if err := k8s.Delete(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: SpinnakerDeployerBindingName, Namespace: name}}); err != nil {
		t.Fatal(err)
	}
	foreign := spinnakerRB(name)
	foreign.RoleRef.Name = "cluster-admin"
	if err := k8s.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	reconcileOnceWith(t, op, name, true)
	reconcileOnceWith(t, op, name, true)
	if rb := get(t, &rbacv1.RoleBinding{}, name, SpinnakerDeployerBindingName); rb.RoleRef.Name != "cluster-admin" || len(rb.OwnerReferences) != 0 {
		t.Errorf("uncontrolled binding was touched: %+v owners %+v", rb.RoleRef, rb.OwnerReferences)
	}
	if err := k8s.Delete(ctx, foreign); err != nil {
		t.Fatal(err)
	}

	// A controlled binding with a foreign roleRef (immutable) is deleted and recreated.
	controlled := spinnakerRB(name)
	controlled.RoleRef.Name = "cluster-admin"
	if err := controllerutil.SetControllerReference(getTN(t, name), controlled, testScheme); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(ctx, controlled); err != nil {
		t.Fatal(err)
	}
	reconcileOnceWith(t, op, name, true)
	reconcileOnceWith(t, op, name, true)
	if rb := get(t, &rbacv1.RoleBinding{}, name, SpinnakerDeployerBindingName); rb.RoleRef != spinnakerRoleRef() {
		t.Errorf("roleRef not repaired: %+v", rb.RoleRef)
	}

	// Flag off again: the controlled binding is removed.
	reconcileOnceWith(t, op, name, false)
	absent("flag turned off")
	assertStatus(t, getTN(t, name), idpv1alpha1.PhaseReady, ReasonReconciled)

	// A binding of that name the TeamNamespace does not control is left alone.
	if err := k8s.Create(ctx, spinnakerRB(name)); err != nil {
		t.Fatal(err)
	}
	reconcileOnceWith(t, op, name, false)
	get(t, &rbacv1.RoleBinding{}, name, SpinnakerDeployerBindingName)
}

// TestTenantServicesPolicy: in operator-managed namespaces any user may create
// only plain ClusterIP Services (the workload HTTPRoute forwards to the Service
// named like the workload); other namespaces are untouched.
func TestTenantServicesPolicy(t *testing.T) {
	_ = operatorClient(t)
	managed, plain := "team-svc-guard", "plain-svc-ns"
	if err := k8s.Create(ctx, newTN(managed)); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, k8s, managed)
	ensurePlainNamespace(t, plain)
	dry := client.DryRunAll
	svc := func(ns string, mut func(*corev1.Service)) *corev1.Service {
		s := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
			Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}}
		if mut != nil {
			mut(s)
		}
		return s
	}
	denied := func(err error) bool {
		return apierrors.IsForbidden(err) && strings.Contains(err.Error(), "idp-tenant-services")
	}
	eventually(t, "tenant services policy active", func() error {
		if err := k8s.Create(ctx, svc(managed, func(s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeLoadBalancer }), dry); !denied(err) {
			return fmt.Errorf("probe: %v", err)
		}
		return nil
	})
	for what, mut := range map[string]func(*corev1.Service){
		"ExternalName": func(s *corev1.Service) {
			s.Spec.Type, s.Spec.ExternalName, s.Spec.Ports = corev1.ServiceTypeExternalName, "keycloak.keycloak.svc.cluster.local", nil
		},
		"LoadBalancer": func(s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeLoadBalancer },
		"NodePort":     func(s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeNodePort },
		"externalIPs":  func(s *corev1.Service) { s.Spec.ExternalIPs = []string{"10.1.2.3"} },
		"ClusterIP+extIP": func(s *corev1.Service) {
			s.Spec.Type, s.Spec.ExternalIPs = corev1.ServiceTypeClusterIP, []string{"10.1.2.3"}
		},
	} {
		if err := k8s.Create(ctx, svc(managed, mut), dry); !denied(err) {
			t.Errorf("create %s in managed ns: want denial, got %v", what, err)
		}
	}
	for what, mut := range map[string]func(*corev1.Service){
		"no type":   nil,
		"ClusterIP": func(s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeClusterIP },
		"headless":  func(s *corev1.Service) { s.Spec.ClusterIP = corev1.ClusterIPNone },
	} {
		if err := k8s.Create(ctx, svc(managed, mut), dry); err != nil {
			t.Errorf("create %s in managed ns: %v", what, err)
		}
	}
	// UPDATE: a stored ClusterIP Service cannot be turned into an ExternalName/LoadBalancer.
	if err := k8s.Create(ctx, svc(managed, nil)); err != nil {
		t.Fatal(err)
	}
	cur := get(t, &corev1.Service{}, managed, "web")
	cur.Spec.Type = corev1.ServiceTypeLoadBalancer
	if err := k8s.Update(ctx, cur, dry); !denied(err) {
		t.Errorf("update to LoadBalancer: want denial, got %v", err)
	}
	cur = get(t, &corev1.Service{}, managed, "web")
	cur.Spec.ExternalIPs = []string{"10.1.2.3"}
	if err := k8s.Update(ctx, cur, dry); !denied(err) {
		t.Errorf("update with externalIPs: want denial, got %v", err)
	}
	// Unmanaged namespaces: any type.
	for _, typ := range []corev1.ServiceType{corev1.ServiceTypeLoadBalancer, corev1.ServiceTypeNodePort} {
		if err := k8s.Create(ctx, svc(plain, func(s *corev1.Service) { s.Spec.Type = typ }), dry); err != nil {
			t.Errorf("create %s in unmanaged ns: %v", typ, err)
		}
	}
	if err := k8s.Create(ctx, svc(plain, func(s *corev1.Service) {
		s.Spec.Type, s.Spec.ExternalName, s.Spec.Ports = corev1.ServiceTypeExternalName, "example.com", nil
	}), dry); err != nil {
		t.Errorf("create ExternalName in unmanaged ns: %v", err)
	}
}
