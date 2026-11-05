package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Access levels of a ResourceGrant. The broker maps them to a fixed role menu
// in code: reader = roles/storage.objectViewer, writer = roles/storage.objectUser.
const (
	AccessReader = "reader"
	AccessWriter = "writer"
)

// ResourceGrantSpec is immutable: a change means delete and re-create.
type ResourceGrantSpec struct {
	// Workload is the name of a WorkloadIdentity in the same namespace; its
	// ServiceAccount is the principal that gets access. Same rules as a
	// WorkloadIdentity name.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workload is immutable"
	// +kubebuilder:validation:XValidation:rule="self.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="workload must be a DNS-1123 label"
	// +kubebuilder:validation:XValidation:rule="!(self in ['vault-reader', 'default']) && !['default-token', 'vault-reader-token', 'sh-helm', 'kube-', 'istio-', 'idp-'].exists(p, self.startsWith(p))",message="workload name is reserved"
	Workload string `json:"workload"`
	// Resource is the name of a BucketClaim in the same namespace.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=18
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="resource is immutable"
	// +kubebuilder:validation:XValidation:rule="self.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="resource must be a DNS-1123 label"
	Resource string `json:"resource"`
	// Access is the fixed menu entry: reader or writer.
	// +required
	// +kubebuilder:validation:Enum=reader;writer
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="access is immutable"
	Access string `json:"access"`
}

type ResourceGrantStatus struct {
	// Member is the IAM principal the grant binds, informational:
	// principal://iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/<project>.svc.id.goog/subject/ns/<namespace>/sa/<workload>.
	// +optional
	Member string `json:"member,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ResourceGrant gives the workload spec.workload (its WorkloadIdentity
// ServiceAccount, as a direct principal:// IAM member, no key and no Google
// service account) reader or writer access to the bucket of the BucketClaim
// spec.resource in the same namespace (ADR 0010). The broker keeps the bucket's
// IAM bindings for that namespace equal to the set of all non-terminating
// grants of the claim; it touches no other binding. Deleting the grant removes
// the binding.
//
// The spec is immutable (workload, resource, access): delete and re-create to
// change it. metadata.name is free (DNS-1123 label, at most 63 characters).
//
// Status: Ready=True (reason Bound) means the binding is in place. Ready=False
// reasons: NotManaged (the namespace is not managed), WorkloadNotFound,
// ClaimNotFound, ClaimNotReady (including a bucket the claim does not own),
// GrantFailed (the IAM call failed).
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=rgrant
// +kubebuilder:printcolumn:name="Workload",type=string,JSONPath=`.spec.workload`
// +kubebuilder:printcolumn:name="Resource",type=string,JSONPath=`.spec.resource`
// +kubebuilder:printcolumn:name="Access",type=string,JSONPath=`.spec.access`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 63",message="name must be at most 63 characters"
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="name must be a DNS-1123 label"
type ResourceGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec ResourceGrantSpec `json:"spec"`
	// +optional
	Status ResourceGrantStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ResourceGrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceGrant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ResourceGrant{}, &ResourceGrantList{})
}
