package delivery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
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
	ctx        = context.Background()
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
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		env.BinaryAssetsDirectory = filepath.Join("..", "..", "bin", "k8s",
			fmt.Sprintf("%s-%s-%s", envtestK8sVersion, runtime.GOOS, runtime.GOARCH))
	}

	var err error
	cfg, err = env.Start()
	if err != nil {
		envReason = fmt.Sprintf("envtest unavailable (run `make envtest`): %v", err)
	} else if k8s, err = client.New(cfg, client.Options{Scheme: testScheme}); err != nil {
		panic(err)
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
