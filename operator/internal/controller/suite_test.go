package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

const envtestK8sVersion = "1.37.0"

var (
	testScheme = k8sruntime.NewScheme()
	cfg        *rest.Config
	k8s        client.Client
	envReason  string
)

func TestMain(m *testing.M) {
	if err := clientgoscheme.AddToScheme(testScheme); err != nil {
		panic(err)
	}
	if err := idpv1alpha1.AddToScheme(testScheme); err != nil {
		panic(err)
	}
	ctrl.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	// envtest already runs with --authorization-mode=RBAC and the default admission
	// plugins (ValidatingAdmissionPolicy included); the admin client is system:masters.
	// OwnerReferencesPermissionEnforcement makes blockOwnerDeletion on child objects
	// require update on teamnamespaces/finalizers, as on clusters that enable it.
	env.ControlPlane.GetAPIServer().Configure().
		Append("enable-admission-plugins", "OwnerReferencesPermissionEnforcement")
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		env.BinaryAssetsDirectory = filepath.Join("..", "..", "bin", "k8s",
			fmt.Sprintf("%s-%s-%s", envtestK8sVersion, runtime.GOOS, runtime.GOARCH))
	}

	var err error
	cfg, err = env.Start()
	if err != nil {
		envReason = fmt.Sprintf("envtest unavailable (run `make envtest`): %v", err)
	} else {
		k8s, err = client.New(cfg, client.Options{Scheme: testScheme})
		if err != nil {
			panic(err)
		}
	}

	code := m.Run()
	if cfg != nil {
		_ = env.Stop()
	}
	os.Exit(code)
}

func requireEnvtest(t *testing.T) {
	t.Helper()
	if k8s == nil {
		t.Skip(envReason)
	}
}

func newTN(name string) *idpv1alpha1.TeamNamespace {
	return &idpv1alpha1.TeamNamespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: idpv1alpha1.TeamNamespaceSpec{
			BusinessUnit: "payments",
			Owner:        "dev@example.com",
			OrderID:      "ORD-2026-000123",
		},
	}
}

func reconcileOnce(t *testing.T, c client.Client, name string) ctrl.Result {
	t.Helper()
	r := &TeamNamespaceReconciler{Client: c, Scheme: testScheme}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
	return res
}

func eventually(t *testing.T, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = fn(); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s: %v", what, err)
}
