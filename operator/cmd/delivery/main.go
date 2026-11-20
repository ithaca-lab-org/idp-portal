package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
	"github.com/ithaca-lab-org/idp-portal/operator/internal/delivery"
)

// The delivery controller of ADR 0011: a separate Deployment (idp-system/idp-delivery)
// with its own image entry point and ServiceAccount, and the only caller of
// Spinnaker's Gate. It shares nothing with the operator manager or the broker
// but the API types.

func main() {
	var (
		metricsAddr string
		probeAddr   string
		leaderElect bool
		enabled     bool
		gateURL     string
		orgs        string
	)
	enabledDefault, err := strconv.ParseBool(envOr("DELIVERY_ENABLED", "false"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid DELIVERY_ENABLED:", err)
		os.Exit(1)
	}
	flag.BoolVar(&enabled, "enabled", enabledDefault, "Run the controllers; false starts an idle process that makes no Gate call (env DELIVERY_ENABLED).")
	flag.StringVar(&gateURL, "gate-url", envOr("GATE_URL", "http://gate.spinnaker.svc:8084"), "Spinnaker Gate base URL; /api/v1 is appended when it has no path (env GATE_URL).")
	flag.StringVar(&orgs, "allowed-repo-orgs", envOr("DELIVERY_ALLOWED_REPO_ORGS", delivery.DefaultAllowedOrgs), "Comma separated GitHub owners a DeliveryPipeline repository may belong to (env DELIVERY_ALLOWED_REPO_ORGS).")
	flag.StringVar(&metricsAddr, "metrics-bind-address", envOr("METRICS_BIND_ADDRESS", "0"), "Metrics endpoint address, plain HTTP, no authn; \"0\" disables it (env METRICS_BIND_ADDRESS).")
	flag.StringVar(&probeAddr, "health-probe-bind-address", envOr("HEALTH_PROBE_BIND_ADDRESS", ":8081"), "Health and readiness probe address (env HEALTH_PROBE_BIND_ADDRESS).")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for high availability.")
	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	// controller-runtime allows the signal handler to be installed ONCE (a second call panics with
	// "close of closed channel"): everything shares this context.
	ctx := ctrl.SetupSignalHandler()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	log := ctrl.Log.WithName("setup")

	allowed := delivery.ParseOrgs(orgs)
	var gate delivery.Gate
	if enabled {
		if len(allowed) == 0 {
			log.Error(fmt.Errorf("allowed-repo-orgs is empty"), "invalid delivery configuration")
			os.Exit(1)
		}
		g, err := delivery.NewHTTPGate(gateURL)
		if err != nil {
			log.Error(err, "invalid delivery configuration")
			os.Exit(1)
		}
		gate = g
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(idpv1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "idp-delivery",
		// The lease lives next to the controller; the Role in delivery-role.yaml is
		// limited to idp-system.
		LeaderElectionNamespace: "idp-system",
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if enabled {
		cfg := delivery.Config{Gate: gate, AllowedOrgs: allowed, Recorder: mgr.GetEventRecorder("idp-delivery")}
		if err := delivery.NewPipelineReconciler(mgr.GetClient(), cfg).SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up controller", "controller", "DeliveryPipeline")
			os.Exit(1)
		}
		if err := delivery.NewRunReconciler(mgr.GetClient(), cfg).SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up controller", "controller", "DeliveryRun")
			os.Exit(1)
		}
		log.Info("delivery controller enabled", "allowedOrgs", allowed)
	} else {
		log.Info("delivery controller disabled (--enabled=false): no controllers, no Gate calls")
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
	log.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "manager exited with error")
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
