package broker

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

// These tests drive the apiserver as the broker's ServiceAccount (by
// impersonation) with the generated RBAC (config/rbac/broker-role.yaml) and the
// admission policy (config/policy/broker-scope.yaml) installed. Negative cases
// use server-side dry-run, so nothing is persisted if a guard is missing.

const (
	brokerUser = "system:serviceaccount:idp-system:idp-broker"
	brokerPol  = "idp-broker-scope"
)

var (
	brokerOnce   sync.Once
	brokerClient client.Client
	brokerErr    error
)

func brokerGroups() []string {
	return []string{"system:serviceaccounts", "system:serviceaccounts:idp-system", "system:authenticated"}
}

func brokerAPIClient(t *testing.T) client.Client {
	t.Helper()
	requireEnvtest(t)
	brokerOnce.Do(func() { brokerClient, brokerErr = setupBrokerIdentity() })
	if brokerErr != nil {
		t.Fatalf("broker identity setup: %v", brokerErr)
	}
	waitBrokerPolicy(t, brokerClient)
	return brokerClient
}

func setupBrokerIdentity() (client.Client, error) {
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "idp-system"}}); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, err
	}
	for _, f := range []string{
		filepath.Join("..", "..", "config", "rbac", "broker-role.yaml"),
		filepath.Join("..", "..", "config", "policy", "broker-scope.yaml"),
	} {
		if err := applyFile(f); err != nil {
			return nil, err
		}
	}
	sa := rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "idp-broker", Namespace: "idp-system"}
	for _, o := range []client.Object{
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "idp-broker"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "idp-broker"},
			Subjects:   []rbacv1.Subject{sa},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "idp-broker", Namespace: "idp-system"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "idp-broker"},
			Subjects:   []rbacv1.Subject{sa},
		},
	} {
		if err := k8s.Create(ctx, o); err != nil {
			return nil, err
		}
	}
	c := rest.CopyConfig(cfg)
	c.Impersonate = rest.ImpersonationConfig{UserName: brokerUser, Groups: brokerGroups()}
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
			return fmt.Errorf("%s %s: %w", path, u.GetName(), err)
		}
	}
}

func isBrokerDenial(err error) bool {
	return apierrors.IsForbidden(err) && strings.Contains(err.Error(), brokerPol)
}

func wantBrokerDenial(t *testing.T, what string, err error) {
	t.Helper()
	if !isBrokerDenial(err) {
		t.Errorf("%s: want denial by %s, got %v", what, brokerPol, err)
	}
}

// waitBrokerPolicy blocks until the policy is enforced, probing with a dry-run
// update that RBAC allows and only the policy can refuse (a foreign finalizer).
func waitBrokerPolicy(t *testing.T, bc client.Client) {
	t.Helper()
	ns := "policy-probe"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	if err := k8s.Create(ctx, newClaim(ns, "probe")); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	eventually(t, "broker policy active", func() error {
		probe := get(t, &idpv1alpha1.BucketClaim{}, ns, "probe")
		probe.Finalizers = []string{"example.com/probe"}
		if err := bc.Update(ctx, probe, client.DryRunAll); !isBrokerDenial(err) {
			return fmt.Errorf("probe update: %v", err)
		}
		return nil
	})
	p := &admissionv1.ValidatingAdmissionPolicy{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: brokerPol}, p); err != nil {
		t.Fatal(err)
	}
	if tc := p.Status.TypeChecking; tc != nil && len(tc.ExpressionWarnings) > 0 {
		t.Fatalf("policy type-check warnings: %+v", tc.ExpressionWarnings)
	}
}

// TestBrokerRBACIsExactlyTheMenu checks the generated ClusterRole and Role with
// SubjectAccessReviews, so a widened marker fails here.
func TestBrokerRBACIsExactlyTheMenu(t *testing.T) {
	brokerAPIClient(t)
	const g = "idp.ithaca-idp.com"
	type check struct {
		verb, group, resource, sub string
		ns                         string
		want                       bool
	}
	var checks []check
	for _, r := range []string{"bucketclaims", "resourcegrants"} {
		for _, v := range []string{"get", "list", "watch", "update"} {
			checks = append(checks, check{v, g, r, "", "team-x", true})
		}
		for _, v := range []string{"create", "delete", "deletecollection", "patch"} {
			checks = append(checks, check{v, g, r, "", "team-x", false})
		}
		checks = append(checks,
			check{"update", g, r, "status", "team-x", true}, check{"get", g, r, "status", "team-x", true},
			check{"patch", g, r, "status", "team-x", false}, check{"create", g, r, "status", "team-x", false},
			check{"update", g, r, "finalizers", "team-x", true}, check{"get", g, r, "finalizers", "team-x", false})
	}
	for _, r := range []string{"workloadidentities", "teamnamespaces"} {
		for _, v := range []string{"get", "list", "watch"} {
			checks = append(checks, check{v, g, r, "", "team-x", true})
		}
		for _, v := range []string{"create", "update", "patch", "delete"} {
			checks = append(checks, check{v, g, r, "", "team-x", false})
		}
		checks = append(checks, check{"update", g, r, "status", "team-x", false})
	}
	for _, v := range []string{"get", "list", "watch"} {
		checks = append(checks, check{v, "", "namespaces", "", "", true})
	}
	for _, v := range []string{"create", "update", "patch", "delete"} {
		checks = append(checks, check{v, "", "namespaces", "", "", false})
	}
	checks = append(checks,
		check{"create", "events.k8s.io", "events", "", "team-x", true}, check{"patch", "events.k8s.io", "events", "", "team-x", true},
		check{"delete", "events.k8s.io", "events", "", "team-x", false},
		check{"get", "", "secrets", "", "team-x", false}, check{"list", "", "secrets", "", "", false},
		check{"get", "", "serviceaccounts", "", "team-x", false}, check{"create", "", "serviceaccounts", "", "team-x", false},
		check{"get", "", "configmaps", "", "team-x", false},
		check{"create", "rbac.authorization.k8s.io", "rolebindings", "", "team-x", false},
		check{"create", "authentication.k8s.io", "tokenreviews", "", "", false},
		check{"get", "idp.ithaca-idp.com", "managedsecrets", "", "team-x", false},
		check{"get", "coordination.k8s.io", "leases", "", "idp-system", true},
		check{"create", "coordination.k8s.io", "leases", "", "idp-system", true},
		check{"update", "coordination.k8s.io", "leases", "", "idp-system", true},
		check{"delete", "coordination.k8s.io", "leases", "", "idp-system", false},
		check{"get", "coordination.k8s.io", "leases", "", "team-x", false})
	for _, c := range checks {
		sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User: brokerUser, Groups: brokerGroups(),
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: c.ns, Verb: c.verb, Group: c.group, Resource: c.resource, Subresource: c.sub,
			},
		}}
		if err := k8s.Create(ctx, sar); err != nil {
			t.Fatal(err)
		}
		if sar.Status.Allowed != c.want {
			t.Errorf("%s %s/%s sub=%q in %q: allowed=%v, want %v", c.verb, c.group, c.resource, c.sub, c.ns, sar.Status.Allowed, c.want)
		}
	}
}

func TestBrokerPolicyUpdatesOnly(t *testing.T) {
	bc := brokerAPIClient(t)
	ns := "policy-upd"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	claim := newClaim(ns, "data")
	claim.Finalizers = []string{BucketFinalizerName}
	claim.Labels = map[string]string{"a": "b"}
	if err := k8s.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	grant := newGrant(ns, "g1", "web", "data", "reader")
	grant.Finalizers = []string{GrantFinalizerName}
	if err := k8s.Create(ctx, grant); err != nil {
		t.Fatal(err)
	}
	dry := client.DryRunAll

	// Allowed: finalizers of the kind's own name, status.
	c := get(t, &idpv1alpha1.BucketClaim{}, ns, "data")
	if err := bc.Update(ctx, c, dry); err != nil {
		t.Errorf("no-op update: %v", err)
	}
	c.Finalizers = nil
	if err := bc.Update(ctx, c, dry); err != nil {
		t.Errorf("removing the finalizer: %v", err)
	}
	c = get(t, &idpv1alpha1.BucketClaim{}, ns, "data")
	c.Status.BucketName = "idp-policy-upd-data-0123abcd"
	meta.SetStatusCondition(&c.Status.Conditions, readyCondition(1, metav1.ConditionTrue, ReasonProvisioned, "ok"))
	if err := bc.Status().Update(ctx, c, dry); err != nil {
		t.Errorf("status update: %v", err)
	}
	c.Status.BucketName = ""
	if err := bc.Status().Update(ctx, c, dry); err != nil {
		t.Errorf("status update with an empty bucketName: %v", err)
	}
	g := get(t, &idpv1alpha1.ResourceGrant{}, ns, "g1")
	g.Status.Member = "principal://x"
	meta.SetStatusCondition(&g.Status.Conditions, readyCondition(1, metav1.ConditionTrue, ReasonBound, "ok"))
	if err := bc.Status().Update(ctx, g, dry); err != nil {
		t.Errorf("grant status update: %v", err)
	}

	// Foreign finalizers (foregroundDeletion, a tenant's) may be present: the
	// broker still works on the object, but never adds or removes them.
	mixed := newClaim(ns, "mixed")
	mixed.Finalizers = []string{"example.com/tenant", BucketFinalizerName, "foregroundDeletion"}
	if err := k8s.Create(ctx, mixed); err != nil {
		t.Fatal(err)
	}
	m := get(t, &idpv1alpha1.BucketClaim{}, ns, "mixed")
	if err := bc.Update(ctx, m, dry); err != nil {
		t.Errorf("no-op update with foreign finalizers: %v", err)
	}
	m.Status.BucketName = ""
	meta.SetStatusCondition(&m.Status.Conditions, readyCondition(1, metav1.ConditionFalse, ReasonNameTaken, "x"))
	if err := bc.Status().Update(ctx, m, dry); err != nil {
		t.Errorf("status update with foreign finalizers: %v", err)
	}
	m.Finalizers = []string{"example.com/tenant", "foregroundDeletion"}
	if err := bc.Update(ctx, m, dry); err != nil {
		t.Errorf("removing only our finalizer next to foreign ones: %v", err)
	}
	m.Finalizers = []string{"example.com/tenant", BucketFinalizerName}
	wantBrokerDenial(t, "removing foregroundDeletion", bc.Update(ctx, m, dry))
	m.Finalizers = []string{BucketFinalizerName, "foregroundDeletion"}
	wantBrokerDenial(t, "removing a tenant finalizer", bc.Update(ctx, m, dry))
	m.Finalizers = []string{"example.com/tenant", BucketFinalizerName, "foregroundDeletion", "example.com/new"}
	wantBrokerDenial(t, "adding a foreign finalizer next to foreign ones", bc.Update(ctx, m, dry))

	// Denied by the policy.
	c = get(t, &idpv1alpha1.BucketClaim{}, ns, "data")
	c.Finalizers = []string{BucketFinalizerName, "example.com/other"}
	wantBrokerDenial(t, "foreign finalizer on a claim", bc.Update(ctx, c, dry))
	c.Finalizers = []string{GrantFinalizerName}
	wantBrokerDenial(t, "the grant finalizer on a claim", bc.Update(ctx, c, dry))
	g = get(t, &idpv1alpha1.ResourceGrant{}, ns, "g1")
	g.Finalizers = []string{BucketFinalizerName}
	wantBrokerDenial(t, "the bucket finalizer on a grant", bc.Update(ctx, g, dry))
	g.Finalizers = []string{GrantFinalizerName, "example.com/other"}
	wantBrokerDenial(t, "foreign finalizer on a grant", bc.Update(ctx, g, dry))

	c = get(t, &idpv1alpha1.BucketClaim{}, ns, "data")
	c.Labels = map[string]string{"a": "changed"}
	wantBrokerDenial(t, "label change", bc.Update(ctx, c, dry))
	c = get(t, &idpv1alpha1.BucketClaim{}, ns, "data")
	c.Labels = nil
	wantBrokerDenial(t, "label removal", bc.Update(ctx, c, dry))
	c = get(t, &idpv1alpha1.BucketClaim{}, ns, "data")
	c.Annotations = map[string]string{"x": "y"}
	wantBrokerDenial(t, "annotation", bc.Update(ctx, c, dry))
	c = get(t, &idpv1alpha1.BucketClaim{}, ns, "data")
	c.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "x", UID: "11111111-1111-1111-1111-111111111111"}}
	// The apiserver's OwnerReferencesPermissionEnforcement may refuse first; either way it is Forbidden.
	if err := bc.Update(ctx, c, dry); !apierrors.IsForbidden(err) {
		t.Errorf("ownerReference: want Forbidden, got %v", err)
	}

	c = get(t, &idpv1alpha1.BucketClaim{}, ns, "data")
	c.Status.BucketName = "idp-other-ns-data-0123abcd"
	wantBrokerDenial(t, "status.bucketName of another bucket", bc.Status().Update(ctx, c, dry))
	c.Status.BucketName = "idp-policy-upd-data2-0123abcd"
	wantBrokerDenial(t, "status.bucketName of another claim", bc.Status().Update(ctx, c, dry))
	for _, bad := range []string{"idp-policy-upd-data", "idp-policy-upd-data-0123abc", "idp-policy-upd-data-0123abcde", "idp-policy-upd-data-0123ABCD", "idp-policy-upd-data-zzzzzzzz"} {
		c.Status.BucketName = bad
		wantBrokerDenial(t, "status.bucketName "+bad, bc.Status().Update(ctx, c, dry))
	}

	// The spec of a grant is immutable by the CRD whoever asks.
	g = get(t, &idpv1alpha1.ResourceGrant{}, ns, "g1")
	g.Spec.Access = "writer"
	if err := bc.Update(ctx, g, dry); err == nil {
		t.Error("grant spec change accepted")
	}

	// Without the policy these writes would pass RBAC.
	binding := &admissionv1.ValidatingAdmissionPolicyBinding{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: brokerPol}, binding); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Delete(ctx, binding); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		restored := &admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: brokerPol}, Spec: *binding.Spec.DeepCopy()}
		if err := k8s.Create(ctx, restored); err != nil {
			t.Fatal(err)
		}
		waitBrokerPolicy(t, bc)
	})
	eventually(t, "policy binding gone", func() error {
		c := get(t, &idpv1alpha1.BucketClaim{}, ns, "data")
		c.Labels = map[string]string{"a": "changed"}
		if err := bc.Update(ctx, c, dry); err != nil {
			return err
		}
		return nil
	})
}

// With create and delete added to its RBAC the broker is still refused by the policy.
func TestBrokerPolicyDeniesCreateAndDelete(t *testing.T) {
	bc := brokerAPIClient(t)
	ns := "policy-cd"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(ctx, newClaim(ns, "data")); err != nil {
		t.Fatal(err)
	}
	wide := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "idp-broker-test-wide"}, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{idpv1alpha1.GroupVersion.Group},
		Resources: []string{"bucketclaims", "resourcegrants", "bucketclaims/status", "resourcegrants/status"},
		Verbs:     []string{"create", "delete", "deletecollection", "patch"},
	}}}
	wideB := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "idp-broker-test-wide"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: wide.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "idp-broker", Namespace: "idp-system"}},
	}
	for _, o := range []client.Object{wide, wideB} {
		if err := k8s.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8s.Delete(ctx, wideB)
		_ = k8s.Delete(ctx, wide)
	})
	dry := client.DryRunAll
	eventually(t, "wide RBAC active", func() error {
		err := bc.Create(ctx, newClaim(ns, "new"), dry)
		if !isBrokerDenial(err) {
			return fmt.Errorf("create: %v", err)
		}
		return nil
	})
	wantBrokerDenial(t, "create claim", bc.Create(ctx, newClaim(ns, "new"), dry))
	wantBrokerDenial(t, "create grant", bc.Create(ctx, newGrant(ns, "g", "web", "data", "reader"), dry))
	wantBrokerDenial(t, "delete claim", bc.Delete(ctx, newClaim(ns, "data"), dry))
	wantBrokerDenial(t, "deletecollection", bc.DeleteAllOf(ctx, &idpv1alpha1.BucketClaim{}, client.InNamespace(ns), dry))
	wantBrokerDenial(t, "patch claim", bc.Patch(ctx, newClaim(ns, "data"), client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"labels":{"x":"y"}}}`)), dry))
	wantBrokerDenial(t, "patch status", bc.Status().Patch(ctx, newClaim(ns, "data"), client.RawPatch(types.MergePatchType, []byte(`{"status":{"bucketName":"idp-x"}}`)), dry))
}

// The reconcilers run as the broker's ServiceAccount, under its real RBAC and
// the policy, through a full claim and grant lifecycle.
func TestBrokerLifecycleUnderRBACAndPolicy(t *testing.T) {
	bc := brokerAPIClient(t)
	ns := "bk-life"
	managedNamespace(t, ns)
	f := newFakeCloud()
	cr := NewBucketClaimReconciler(bc, testConfig(f))
	gr := NewResourceGrantReconciler(bc, testConfig(f))
	if err := k8s.Create(ctx, newClaim(ns, "data")); err != nil {
		t.Fatal(err)
	}
	newWI(t, ns, "web")
	if err := k8s.Create(ctx, newGrant(ns, "g1", "web", "data", "writer")); err != nil {
		t.Fatal(err)
	}
	reconcileClaim(t, cr, ns, "data")
	reconcileGrant(t, gr, ns, "g1")
	wantReady(t, claimReady(t, ns, "data"), metav1.ConditionTrue, ReasonProvisioned)
	wantReady(t, grantReady(t, ns, "g1"), metav1.ConditionTrue, ReasonBound)
	b := f.bucket(bn(ns, "data"))
	if len(b.members(RoleWriter)) != 1 {
		t.Fatalf("writers = %v", b.members(RoleWriter))
	}

	deleteClaim(t, ns, "data")
	if err := k8s.Delete(ctx, newGrant(ns, "g1", "web", "data", "writer")); err != nil {
		t.Fatal(err)
	}
	reconcileGrant(t, gr, ns, "g1")
	reconcileClaim(t, cr, ns, "data")
	if !claimGone(t, ns, "data") {
		t.Error("claim finalizer not removed under the policy")
	}
	if f.bucket(bn(ns, "data")) != nil {
		t.Error("empty bucket kept")
	}
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "g1"}, &idpv1alpha1.ResourceGrant{}); !apierrors.IsNotFound(err) {
		t.Errorf("grant finalizer not removed: %v", err)
	}
}
