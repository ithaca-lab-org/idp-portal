package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionSynced reports whether the VaultStaticSecret for a ManagedSecret
// exists with the operator's fixed spec.
const ConditionSynced = "Synced"

// ManagedSecretSpec is intentionally empty: everything about the
// VaultStaticSecret follows from the namespace and name, so the portal (the
// only writer) cannot steer it. Unknown fields are pruned.
type ManagedSecretSpec struct{}

type ManagedSecretStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ManagedSecret asks the operator to sync the Vault KV v2 secret
// kv/<namespace>/<name> into the Kubernetes Secret <name> in the same
// namespace, through a VSO VaultStaticSecret <name> that uses the namespace's
// VaultAuth vault-reader. It only acts in namespaces managed by a
// TeamNamespace and only with VAULT_ENABLED=true. The secret value never
// passes through this resource.
//
// Names: DNS-1123 label, at most 40 characters, and not one that could shadow
// platform or legacy Secrets (vault-reader, default, or the prefixes
// default-token, vault-reader-token, sh-helm, kube-, istio-, idp-).
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=msec
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 40",message="name must be at most 40 characters"
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="name must be a DNS-1123 label"
// +kubebuilder:validation:XValidation:rule="!(self.metadata.name in ['vault-reader', 'default']) && !['default-token', 'vault-reader-token', 'sh-helm', 'kube-', 'istio-', 'idp-'].exists(p, self.metadata.name.startsWith(p))",message="name is reserved"
type ManagedSecret struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec ManagedSecretSpec `json:"spec,omitempty"`
	// +optional
	Status ManagedSecretStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ManagedSecretList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ManagedSecret `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ManagedSecret{}, &ManagedSecretList{})
}
