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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
		ns.Labels["team/extra"] = "ok"
		if err := op.Update(ctx, ns, dry); err != nil {
			t.Errorf("benign update of managed namespace: %v", err)
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

type purgerFunc func(ctx context.Context, ns string) error

func (f purgerFunc) Purge(ctx context.Context, ns string) error { return f(ctx, ns) }

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
		Purger: purgerFunc(func(_ context.Context, ns string) error { purged = append(purged, ns); return nil })}
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
