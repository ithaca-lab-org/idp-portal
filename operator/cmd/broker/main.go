package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
	"github.com/ithaca-lab-org/idp-portal/operator/internal/broker"
)

// The resource broker of ADR 0010: a separate Deployment (idp-system/idp-broker)
// with its own image entry point, ServiceAccount and Google service account.
// It shares nothing with the operator manager but the API types.

var locationRe = regexp.MustCompile(`^[a-z]+(-[a-z0-9]+)*$`)

func main() {
	var (
		metricsAddr string
		probeAddr   string
		leaderElect bool
		enabled     bool
		projectID   string
		projectNum  string
		location    string
		giveUp      time.Duration
	)
	enabledDefault, err := strconv.ParseBool(envOr("BROKER_ENABLED", "false"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid BROKER_ENABLED:", err)
		os.Exit(1)
	}
	giveUpDefault, err := time.ParseDuration(envOr("BROKER_GIVE_UP_AFTER", broker.DefaultGiveUpAfter.String()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid BROKER_GIVE_UP_AFTER:", err)
		os.Exit(1)
	}
	flag.BoolVar(&enabled, "enabled", enabledDefault, "Run the controllers; false starts an idle process that makes no Google call (env BROKER_ENABLED).")
	flag.StringVar(&projectID, "project-id", envOr("PROJECT_ID", ""), "Google project ID: owns the buckets and names the workload identity pool (env PROJECT_ID).")
	flag.StringVar(&projectNum, "project-number", envOr("PROJECT_NUMBER", ""), "Google project number, used in principal:// members (env PROJECT_NUMBER).")
	flag.StringVar(&location, "bucket-location", envOr("BUCKET_LOCATION", "us-central1"), "Location of new buckets (env BUCKET_LOCATION).")
	flag.DurationVar(&giveUp, "give-up-after", giveUpDefault, "How long a failing clean-up may hold a finalizer once deletion started (a claim) or its namespace is going away (a grant) (env BROKER_GIVE_UP_AFTER).")
	flag.StringVar(&metricsAddr, "metrics-bind-address", envOr("METRICS_BIND_ADDRESS", "0"), "Metrics endpoint address, plain HTTP, no authn; \"0\" disables it (env METRICS_BIND_ADDRESS).")
	flag.StringVar(&probeAddr, "health-probe-bind-address", envOr("HEALTH_PROBE_BIND_ADDRESS", ":8081"), "Health and readiness probe address (env HEALTH_PROBE_BIND_ADDRESS).")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for high availability.")
	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	// controller-runtime allows the signal handler to be installed ONCE (a second call panics with
	// "close of closed channel"): the Google client and the manager share this context.
	ctx := ctrl.SetupSignalHandler()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	log := ctrl.Log.WithName("setup")

	if enabled {
		if err := validate(projectID, projectNum, location, giveUp); err != nil {
			log.Error(err, "invalid broker configuration")
			os.Exit(1)
		}
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(idpv1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "idp-broker",
		// The lease lives next to the broker; the Role in broker-role.yaml is
		// limited to idp-system.
		LeaderElectionNamespace: "idp-system",
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if enabled {
		cloud, err := broker.NewGCS(ctx, projectID, projectNum, location)
		if err != nil {
			log.Error(err, "unable to create the Google Cloud client")
			os.Exit(1)
		}
		defer cloud.Close()
		cfg := broker.Config{
			Cloud: cloud, ProjectID: projectID, ProjectNumber: projectNum, GiveUpAfter: giveUp,
			Recorder: mgr.GetEventRecorder("idp-broker"),
		}
		if err := broker.NewBucketClaimReconciler(mgr.GetClient(), cfg).SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up controller", "controller", "BucketClaim")
			os.Exit(1)
		}
		if err := broker.NewResourceGrantReconciler(mgr.GetClient(), cfg).SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up controller", "controller", "ResourceGrant")
			os.Exit(1)
		}
		log.Info("resource broker enabled", "project", projectID, "location", location, "giveUpAfter", giveUp.String())
	} else {
		log.Info("resource broker disabled (--enabled=false): no controllers, no Google calls")
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

func validate(projectID, projectNumber, location string, giveUp time.Duration) error {
	if err := (broker.Scope{ProjectNumber: projectNumber, ProjectID: projectID, Namespace: "x"}).Validate(); err != nil {
		return err
	}
	if !locationRe.MatchString(location) {
		return fmt.Errorf("bucket location %q is not a Google location such as us-central1", location)
	}
	if giveUp <= 0 {
		return fmt.Errorf("give-up-after must be positive")
	}
	return nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
