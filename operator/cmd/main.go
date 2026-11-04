package main

import (
	"crypto/tls"
	"flag"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
	"github.com/ithaca-lab-org/idp-portal/operator/internal/controller"
)

// Needed by the authn/authz filter that protects the metrics endpoint.
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:groups=coordination.k8s.io,namespace=idp-system,resources=leases,verbs=get;create;update
// +kubebuilder:rbac:groups="",namespace=idp-system,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,namespace=idp-system,resources=events,verbs=create;patch

func main() {
	var (
		metricsAddr   string
		probeAddr     string
		secureMetrics bool
		leaderElect   bool
		wiOpts        controller.WorkloadIdentityOptions
		gwOpts        controller.GatewayOptions
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8443", "Metrics endpoint address; \"0\" disables it.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Health and readiness probe address.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true, "Serve metrics over HTTPS with Kubernetes authn/authz.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for high availability.")
	flag.StringVar(&wiOpts.TrustDomain, "trust-domain", envOr("TRUST_DOMAIN", controller.DefaultTrustDomain),
		"Istio trust domain for WorkloadIdentity SPIFFE IDs and the gateway principal (env TRUST_DOMAIN).")
	flag.StringVar(&wiOpts.HostnameSuffix, "workload-hostname-suffix", envOr("WORKLOAD_HOSTNAME_SUFFIX", controller.DefaultHostnameSuffix),
		"WorkloadIdentity hostnames are <name>.<namespace>.<suffix> (env WORKLOAD_HOSTNAME_SUFFIX).")
	flag.StringVar(&gwOpts.Namespace, "gateway-namespace", envOr("GATEWAY_NAMESPACE", controller.DefaultGatewayNamespace),
		"Namespace of the ingress Gateway pods admitted by idp-allow-ingress-gateway (env GATEWAY_NAMESPACE).")
	flag.StringVar(&gwOpts.Name, "gateway-name", envOr("GATEWAY_NAME", controller.DefaultGatewayName),
		"Name of the ingress Gateway (label gateway.networking.k8s.io/gateway-name) admitted by idp-allow-ingress-gateway (env GATEWAY_NAME).")
	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	log := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(idpv1alpha1.AddToScheme(scheme))

	metricsOpts := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		// HTTP/2 is disabled to sidestep the rapid-reset class of CVEs.
		TLSOpts: []func(*tls.Config){func(c *tls.Config) { c.NextProtos = []string{"http/1.1"} }},
	}
	if secureMetrics {
		metricsOpts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOpts,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "idp-operator.idp.ithaca-idp.com",
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := wiOpts.Validate(); err != nil {
		log.Error(err, "invalid WorkloadIdentity configuration")
		os.Exit(1)
	}

	if err := gwOpts.Validate(); err != nil {
		log.Error(err, "invalid gateway configuration")
		os.Exit(1)
	}

	// VAULT_ENABLED=false (default) keeps the operator free of any Vault objects,
	// finalizer or calls.
	vaultOpts, err := controller.VaultOptionsFromEnv(os.Getenv)
	if err != nil {
		log.Error(err, "invalid Vault configuration")
		os.Exit(1)
	}
	if vaultOpts != nil {
		log.Info("Vault integration enabled", "addr", vaultOpts.Addr.String(), "purgeRole", vaultOpts.PurgeRole,
			"authMount", vaultOpts.AuthMount, "kvMount", vaultOpts.KVMount, "giveUpAfter", vaultOpts.GiveUpAfter.String(),
			"customCA", vaultOpts.RootCAs != nil)
	}

	if err := (&controller.TeamNamespaceReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Vault:   vaultOpts,
		Gateway: gwOpts,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up controller", "controller", "TeamNamespace")
		os.Exit(1)
	}
	// ManagedSecret -> VaultStaticSecret needs the VSO CRD and Vault access
	// objects, so it only runs with VAULT_ENABLED=true.
	if vaultOpts != nil {
		if err := (&controller.ManagedSecretReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
			Vault:  vaultOpts,
		}).SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up controller", "controller", "ManagedSecret")
			os.Exit(1)
		}
	}

	// WorkloadIdentity needs the Gateway API and Istio CRDs (and the VSO CRD
	// with Vault on); without Vault it creates no VaultAuth. If a CRD is
	// missing the controller is skipped instead of failing the manager (the
	// CRD check runs once at start; restart the operator after installing them).
	missing, err := controller.WorkloadIdentityCRDsMissing(mgr.GetRESTMapper(), vaultOpts != nil)
	switch {
	case err != nil:
		log.Error(err, "unable to check the CRDs for WorkloadIdentity")
		os.Exit(1)
	case len(missing) > 0:
		log.Info("WorkloadIdentity controller disabled: required CRDs are not installed", "missing", missing)
	default:
		if err := (&controller.WorkloadIdentityReconciler{
			Client:  mgr.GetClient(),
			Scheme:  mgr.GetScheme(),
			Options: wiOpts,
			Vault:   vaultOpts,
		}).SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up controller", "controller", "WorkloadIdentity")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	log.Info("starting manager", "gateway", gwOpts.Namespace+"/"+gwOpts.Name, "trustDomain", wiOpts.TrustDomain, "workloadHostnameSuffix", wiOpts.HostnameSuffix)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
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
