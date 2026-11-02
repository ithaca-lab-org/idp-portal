package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkloadIdentitySpec is intentionally empty: the ServiceAccount, hostname,
// route, policy and VaultAuth all follow from the namespace and name, so the
// portal (the only writer) cannot steer them. Unknown fields are pruned.
type WorkloadIdentitySpec struct{}

type WorkloadIdentityStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// SpiffeID is the identity Istio issues to pods running as the
	// ServiceAccount: spiffe://<trust domain>/ns/<namespace>/sa/<name>.
	// +optional
	SpiffeID string `json:"spiffeID,omitempty"`
	// Hostname is reserved for the workload on the ingress Gateway:
	// <name>.<namespace>.<suffix>.
	// +optional
	Hostname string `json:"hostname,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// WorkloadIdentity asks the operator for a per-workload identity in a
// namespace managed by a TeamNamespace (Order #03). Controlled by it (and
// garbage collected with it), all named <name> in the same namespace:
//   - ServiceAccount (Istio issues its SPIFFE ID from it);
//   - Istio AuthorizationPolicy (L4 ALLOW for the namespace and the ingress
//     gateway). It selects pods by label, not by ServiceAccount: workload
//     pods MUST carry idp.ithaca-idp.com/workload=<name> to be covered;
//   - Gateway API HTTPRoute on Gateway istio-ingress/ingress (https) that
//     reserves <name>.<namespace>.<suffix> and has no backend yet;
//   - with VAULT_ENABLED=true, a VSO VaultAuth logging in as the ServiceAccount
//     with Vault role workload-reader, and a finalizer that purges
//     kv/<namespace>/<name>/ on deletion.
//
// Name rules (the portal API must apply the same list): DNS-1123 label
// (^[a-z0-9]([-a-z0-9]*[a-z0-9])?$), at most 40 characters, not exactly
// "vault-reader" or "default", and not starting with "default-token",
// "vault-reader-token", "sh-helm", "kube-", "istio-" or "idp-".
//
// Status: Synced=True means every object above exists with the operator's
// spec. Synced=False reasons: NamespaceNotManaged, NameConflict (an object of
// that name exists that this WorkloadIdentity does not own; it is never
// adopted), PreviousTerminating (one from an earlier WorkloadIdentity of the
// same name is still being deleted), SyncError.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=wid
// +kubebuilder:printcolumn:name="SPIFFE ID",type=string,JSONPath=`.status.spiffeID`
// +kubebuilder:printcolumn:name="Hostname",type=string,JSONPath=`.status.hostname`
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 40",message="name must be at most 40 characters"
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="name must be a DNS-1123 label"
// +kubebuilder:validation:XValidation:rule="!(self.metadata.name in ['vault-reader', 'default']) && !['default-token', 'vault-reader-token', 'sh-helm', 'kube-', 'istio-', 'idp-'].exists(p, self.metadata.name.startsWith(p))",message="name is reserved"
type WorkloadIdentity struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec WorkloadIdentitySpec `json:"spec,omitempty"`
	// +optional
	Status WorkloadIdentityStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type WorkloadIdentityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkloadIdentity `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WorkloadIdentity{}, &WorkloadIdentityList{})
}
