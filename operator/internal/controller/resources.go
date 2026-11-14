package controller

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

const (
	FinalizerName = "idp.ithaca-idp.com/team-namespace"

	LabelManagedBy     = "app.kubernetes.io/managed-by"
	ManagedByValue     = "idp-operator"
	LabelTeamNamespace = "idp.ithaca-idp.com/team-namespace"
	LabelBusinessUnit  = "idp.ithaca-idp.com/business-unit"
	LabelDataplaneMode = "istio.io/dataplane-mode"
	// LabelGatewayAccess lets HTTPRoutes in the namespace attach to the https
	// listener of Gateway istio-ingress/ingress (allowedRoutes selector in
	// idp-gitops workloads/ingress/gateway.yaml). Change both together.
	LabelGatewayAccess = "ithaca.idp/gateway-access"
	GatewayAccessValue = "allowed"

	AnnotationOwner   = "idp.ithaca-idp.com/owner"
	AnnotationOrderID = "idp.ithaca-idp.com/order-id"

	QuotaName             = "idp-quota"
	LimitRangeName        = "idp-limits"
	DenyIngressPolicyName = "idp-default-deny-ingress"
	SameNamespacePolicy   = "idp-allow-same-namespace"
	// GatewayPolicyName admits the ingress Gateway pods (see GatewayOptions).
	GatewayPolicyName    = "idp-allow-ingress-gateway"
	AdminRoleBindingName = "idp-namespace-admin"
	AdminClusterRole     = "admin"

	// SpinnakerDeployerBindingName is the RoleBinding (SPINNAKER_DEPLOYER_ENABLED)
	// that gives Spinnaker's Clouddriver deploy rights in a tenant namespace
	// through the ClusterRole idp-spinnaker-deployer (shipped by idp-gitops).
	SpinnakerDeployerBindingName = "spinnaker-deployer"
	SpinnakerDeployerClusterRole = "idp-spinnaker-deployer"
	SpinnakerNamespace           = "spinnaker"
	SpinnakerServiceAccount      = "clouddriver"
)

var reservedNames = map[string]bool{
	"default": true, "argocd": true, "cert-manager": true, "tailscale": true,
	"identity": true, "idp-system": true, "echo": true, "vault": true,
}

var reservedPrefixes = []string{"kube-", "istio-", "gke-", "vault-"}

func IsReserved(name string) bool {
	if reservedNames[name] {
		return true
	}
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func namespaceLabels(tn *idpv1alpha1.TeamNamespace) map[string]string {
	return map[string]string{
		LabelManagedBy:                       ManagedByValue,
		LabelTeamNamespace:                   tn.Name,
		LabelBusinessUnit:                    tn.Spec.BusinessUnit,
		LabelDataplaneMode:                   "ambient",
		LabelGatewayAccess:                   GatewayAccessValue,
		"pod-security.kubernetes.io/enforce": "baseline",
		"pod-security.kubernetes.io/warn":    "restricted",
	}
}

func childLabels(tn *idpv1alpha1.TeamNamespace) map[string]string {
	return map[string]string{
		LabelManagedBy:     ManagedByValue,
		LabelTeamNamespace: tn.Name,
	}
}

// Object-count quotas for the portal's Resources page (ADR 0010), enforced atomically by the API
// server, so the portal's own count-then-create check cannot be raced past them. They match the
// portal's caps (3 BucketClaims, 10 ResourceGrants per namespace) and are the same for every size.
const (
	QuotaBucketClaims   corev1.ResourceName = "count/bucketclaims.idp.ithaca-idp.com"
	QuotaResourceGrants corev1.ResourceName = "count/resourcegrants.idp.ithaca-idp.com"
)

type sizeProfile struct {
	quota    corev1.ResourceList
	maxLimit corev1.ResourceList
}

var profiles = map[idpv1alpha1.Size]sizeProfile{
	idpv1alpha1.SizeSmall: {
		quota: corev1.ResourceList{
			corev1.ResourceRequestsCPU:            resource.MustParse("2"),
			corev1.ResourceRequestsMemory:         resource.MustParse("4Gi"),
			corev1.ResourceLimitsCPU:              resource.MustParse("4"),
			corev1.ResourceLimitsMemory:           resource.MustParse("8Gi"),
			corev1.ResourceRequestsStorage:        resource.MustParse("20Gi"),
			corev1.ResourcePods:                   resource.MustParse("20"),
			corev1.ResourceServices:               resource.MustParse("10"),
			corev1.ResourcePersistentVolumeClaims: resource.MustParse("5"),
			corev1.ResourceServicesLoadBalancers:  resource.MustParse("0"),
			corev1.ResourceServicesNodePorts:      resource.MustParse("0"),
			QuotaBucketClaims:                     resource.MustParse("3"),
			QuotaResourceGrants:                   resource.MustParse("10"),
		},
		maxLimit: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
	},
	idpv1alpha1.SizeMedium: {
		quota: corev1.ResourceList{
			corev1.ResourceRequestsCPU:            resource.MustParse("4"),
			corev1.ResourceRequestsMemory:         resource.MustParse("8Gi"),
			corev1.ResourceLimitsCPU:              resource.MustParse("8"),
			corev1.ResourceLimitsMemory:           resource.MustParse("16Gi"),
			corev1.ResourceRequestsStorage:        resource.MustParse("50Gi"),
			corev1.ResourcePods:                   resource.MustParse("50"),
			corev1.ResourceServices:               resource.MustParse("20"),
			corev1.ResourcePersistentVolumeClaims: resource.MustParse("10"),
			corev1.ResourceServicesLoadBalancers:  resource.MustParse("0"),
			corev1.ResourceServicesNodePorts:      resource.MustParse("0"),
			QuotaBucketClaims:                     resource.MustParse("3"),
			QuotaResourceGrants:                   resource.MustParse("10"),
		},
		maxLimit: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("8Gi"),
		},
	},
}

func profileFor(s idpv1alpha1.Size) sizeProfile {
	if p, ok := profiles[s]; ok {
		return p
	}
	return profiles[idpv1alpha1.SizeSmall]
}

func desiredQuotaSpec(tn *idpv1alpha1.TeamNamespace) corev1.ResourceQuotaSpec {
	return corev1.ResourceQuotaSpec{Hard: profileFor(tn.Spec.Size).quota.DeepCopy()}
}

func desiredLimitRangeSpec(tn *idpv1alpha1.TeamNamespace) corev1.LimitRangeSpec {
	return corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
		Type: corev1.LimitTypeContainer,
		Max:  profileFor(tn.Spec.Size).maxLimit.DeepCopy(),
		Default: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		DefaultRequest: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}}}
}

func desiredDenyIngressSpec() networkingv1.NetworkPolicySpec {
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
	}
}

func desiredSameNamespaceSpec() networkingv1.NetworkPolicySpec {
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		Ingress: []networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
		}},
	}
}

// GatewayOptions names the ingress Gateway whose pods may reach workload pods.
// Empty fields fall back to the defaults. The admission policy
// (config/policy/operator-scope.yaml) pins the defaults, so a different value
// makes the policy write fail until the policy changes too.
type GatewayOptions struct {
	Namespace string
	Name      string
}

const (
	DefaultGatewayNamespace = "istio-ingress"
	DefaultGatewayName      = "ingress"

	labelGatewayName = "gateway.networking.k8s.io/gateway-name"
	labelNamespaceID = "kubernetes.io/metadata.name"
)

func (g GatewayOptions) withDefaults() GatewayOptions {
	if g.Namespace == "" {
		g.Namespace = DefaultGatewayNamespace
	}
	if g.Name == "" {
		g.Name = DefaultGatewayName
	}
	return g
}

// Validate refuses anything but the defaults: the admission policy and the
// WorkloadIdentity AuthorizationPolicy hard-code istio-ingress/ingress.
func (g GatewayOptions) Validate() error {
	g = g.withDefaults()
	if g.Namespace != DefaultGatewayNamespace || g.Name != DefaultGatewayName {
		return fmt.Errorf("gateway %s/%s: only %s/%s is supported (pinned by the admission policy)",
			g.Namespace, g.Name, DefaultGatewayNamespace, DefaultGatewayName)
	}
	return nil
}

// desiredAllowIngressGatewaySpec admits the ingress Gateway pods to all pods of
// the namespace. Namespace and pod selector sit in ONE peer so they AND (two
// peers would OR, admitting every pod of the gateway namespace or every pod
// with that label anywhere). Only TCP 15008 is open: the gateway reaches
// ambient workloads through HBONE on 15008 only (ADR-0002 check 4 in
// idp-infra/docs/smoke-ambient.md showed a 15008-only policy keeps ambient
// working). Non-ambient tenant pods are intentionally not reachable through
// the gateway.
func desiredAllowIngressGatewaySpec(g GatewayOptions) networkingv1.NetworkPolicySpec {
	g = g.withDefaults()
	tcp, hbone := corev1.ProtocolTCP, intstr.FromInt32(15008)
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		Ingress: []networkingv1.NetworkPolicyIngressRule{{
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &hbone}},
			From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelNamespaceID: g.Namespace}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{labelGatewayName: g.Name}},
			}},
		}},
	}
}

func desiredRoleRef() rbacv1.RoleRef {
	return rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: AdminClusterRole}
}

func desiredSubjects(tn *idpv1alpha1.TeamNamespace) []rbacv1.Subject {
	return []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: tn.Spec.Owner}}
}

func spinnakerRoleRef() rbacv1.RoleRef {
	return rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: SpinnakerDeployerClusterRole}
}

func spinnakerSubjects() []rbacv1.Subject {
	return []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: SpinnakerServiceAccount, Namespace: SpinnakerNamespace}}
}
