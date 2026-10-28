package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:Enum=small;medium
type Size string

const (
	SizeSmall  Size = "small"
	SizeMedium Size = "medium"
)

type Phase string

const (
	PhasePending Phase = "Pending"
	PhaseReady   Phase = "Ready"
	PhaseFailed  Phase = "Failed"
)

const (
	ConditionReady = "Ready"
	// ConditionVaultReady reports the per-namespace Vault access objects
	// (ServiceAccount vault-reader, VaultAuth vault-reader). Only set when the
	// operator runs with VAULT_ENABLED=true.
	ConditionVaultReady = "VaultReady"
	// ConditionVaultPurgePending is True while a deleted TeamNamespace waits for
	// its Vault KV data (kv/<name>/) to be purged.
	ConditionVaultPurgePending = "VaultPurgePending"
)

// +kubebuilder:validation:XValidation:rule="!has(oldSelf.orderId) || (has(self.orderId) && self.orderId == oldSelf.orderId)",message="orderId is immutable once set"
type TeamNamespaceSpec struct {
	// businessUnit owning the namespace; used for cost attribution.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="businessUnit is immutable"
	BusinessUnit string `json:"businessUnit"`

	// owner email; bound to the built-in admin ClusterRole in the namespace.
	// +required
	// +kubebuilder:validation:MaxLength=254
	// +kubebuilder:validation:XValidation:rule="self.matches('^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9-]+([.][a-zA-Z0-9-]+)+$')",message="owner must be an email address"
	Owner string `json:"owner"`

	// size selects the ResourceQuota and LimitRange profile.
	// +optional
	// +kubebuilder:default=small
	Size Size `json:"size,omitempty"`

	// orderId is the portal order that requested this namespace.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9-]*$`
	OrderID string `json:"orderId,omitempty"`
}

type TeamNamespaceStatus struct {
	// +optional
	Phase Phase `json:"phase,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// namespace managed by this resource, set once it exists.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// TeamNamespace requests a guarded namespace whose name equals metadata.name.
// With VAULT_ENABLED=true the operator also adds the finalizer
// idp.ithaca-idp.com/vault-purge, which deletes all Vault KV data under
// kv/<name>/ before the resource goes away. If Vault is unreachable it retries
// (condition VaultPurgePending) and gives up after VAULT_PURGE_GIVE_UP_AFTER
// (default 30m). An admin can skip the purge at once by removing that finalizer
// with a JSON patch on metadata.finalizers (see the operator README).
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=tns
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner`
// +kubebuilder:printcolumn:name="BU",type=string,JSONPath=`.spec.businessUnit`
// +kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.spec.size`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 40",message="name must be at most 40 characters"
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="name must be a DNS-1123 label"
// +kubebuilder:validation:XValidation:rule="!(self.metadata.name in ['default', 'argocd', 'cert-manager', 'tailscale', 'identity', 'idp-system', 'echo']) && !['kube-', 'istio-', 'gke-'].exists(p, self.metadata.name.startsWith(p))",message="name is reserved for the platform"
type TeamNamespace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec TeamNamespaceSpec `json:"spec"`
	// +optional
	Status TeamNamespaceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type TeamNamespaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TeamNamespace `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TeamNamespace{}, &TeamNamespaceList{})
}
