package controller

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

const (
	FinalizerName = "idp.ithaca-idp.com/team-namespace"

	LabelManagedBy     = "app.kubernetes.io/managed-by"
	ManagedByValue     = "idp-operator"
	LabelTeamNamespace = "idp.ithaca-idp.com/team-namespace"
	LabelBusinessUnit  = "idp.ithaca-idp.com/business-unit"
	LabelDataplaneMode = "istio.io/dataplane-mode"

	AnnotationOwner   = "idp.ithaca-idp.com/owner"
	AnnotationOrderID = "idp.ithaca-idp.com/order-id"

	QuotaName             = "idp-quota"
	LimitRangeName        = "idp-limits"
	DenyIngressPolicyName = "idp-default-deny-ingress"
	SameNamespacePolicy   = "idp-allow-same-namespace"
	AdminRoleBindingName  = "idp-namespace-admin"
	AdminClusterRole      = "admin"
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

func desiredRoleRef() rbacv1.RoleRef {
	return rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: AdminClusterRole}
}

func desiredSubjects(tn *idpv1alpha1.TeamNamespace) []rbacv1.Subject {
	return []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: tn.Spec.Owner}}
}
