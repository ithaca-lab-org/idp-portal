package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

func TestIsReserved(t *testing.T) {
	for name, want := range map[string]bool{
		"default": true, "kube-system": true, "kube-public": true, "istio-system": true,
		"gke-managed-system": true, "argocd": true, "cert-manager": true, "tailscale": true,
		"identity": true, "idp-system": true, "echo": true, "vault": true, "vault-demo": true, "vault-secrets-operator": true,
		"payments": false, "kubeflow": false, "echo-team": false, "my-istio-app": false, "vaulted": false, "my-vault": false,
	} {
		if got := IsReserved(name); got != want {
			t.Errorf("IsReserved(%q) = %v, want %v", name, got, want)
		}
	}
}

// The CRD's CEL rule rejects reserved names at admission, so the controller-side
// guard can only be exercised with a client that skips validation.
func TestReservedNameFailsAndCreatesNothing(t *testing.T) {
	for _, name := range []string{"kube-system", "default", "istio-system", "argocd"} {
		t.Run(name, func(t *testing.T) {
			tn := newTN(name)
			c := fake.NewClientBuilder().WithScheme(testScheme).
				WithObjects(tn).WithStatusSubresource(tn).Build()

			reconcileOnce(t, c, name)

			got := &idpv1alpha1.TeamNamespace{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: name}, got); err != nil {
				t.Fatal(err)
			}
			assertStatus(t, got, idpv1alpha1.PhaseFailed, ReasonReservedName)
			if controllerutil.ContainsFinalizer(got, FinalizerName) {
				t.Error("finalizer added for a reserved name")
			}
			for _, list := range []client.ObjectList{
				&corev1.NamespaceList{}, &corev1.ResourceQuotaList{}, &corev1.LimitRangeList{},
				&networkingv1.NetworkPolicyList{}, &rbacv1.RoleBindingList{},
			} {
				if err := c.List(context.Background(), list); err != nil {
					t.Fatal(err)
				}
				items, err := meta.ExtractList(list)
				if err != nil {
					t.Fatal(err)
				}
				if n := len(items); n != 0 {
					t.Errorf("%T has %d items, want 0", list, n)
				}
			}
		})
	}
}

func TestReservedListMatchesCRDRule(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "idp.ithaca-idp.com_teamnamespaces.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rule := string(raw)
	for name := range reservedNames {
		if !strings.Contains(rule, "'"+name+"'") {
			t.Errorf("CRD CEL rule is missing reserved name %q", name)
		}
	}
	for _, p := range reservedPrefixes {
		if !strings.Contains(rule, "'"+p+"'") {
			t.Errorf("CRD CEL rule is missing reserved prefix %q", p)
		}
	}
}
