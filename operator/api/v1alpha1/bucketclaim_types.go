package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BucketClaimSpec is intentionally empty in v1: the bucket name, location and
// settings all follow from the namespace, the claim name and the broker's
// flags, so the portal (the only writer) cannot steer them. Unknown fields are
// pruned.
type BucketClaimSpec struct{}

type BucketClaimStatus struct {
	// BucketName is the Cloud Storage bucket, idp-<namespace>-<name>-<8 hex>.
	// +optional
	BucketName string `json:"bucketName,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// BucketClaim asks the resource broker (a separate Deployment, not the
// operator) for a Cloud Storage bucket idp-<namespace>-<name>-<8 hex of
// sha256(projectNumber/namespace/name)> with uniform
// bucket-level access, public access prevention enforced, no versioning, and
// the labels idp-managed=true, idp-namespace=<namespace>, idp-claim=<name>.
// It only acts in namespaces managed by a TeamNamespace and only with the
// broker enabled (ADR 0010).
//
// Name rules (the portal API must apply the same list): DNS-1123 label
// (^[a-z0-9]([-a-z0-9]*[a-z0-9])?$), at most 18 characters. A bucket name is
// at most 63 characters: "idp-" (4) + namespace (at most 31) + "-" (1) + claim
// (at most 18) + "-" (1) + hash (8) = 63. A namespace of 32 to 40 characters
// (the TeamNamespace limit) cannot own buckets: its claims get InvalidName, as
// does a name that contains "google" or starts with "goog" (Cloud Storage
// refuses those).
//
// Status: Ready=True means the bucket exists, is in the broker's project and
// carries this claim's labels (including the claim's UID: a claim re-created
// under the same name never adopts the old bucket).
// Ready=False reasons: NamespaceNotManaged, InvalidName, NameTaken (a bucket of
// that name exists that this claim does not own, or one this claim orphaned
// earlier; it is never adopted or touched), CreateFailed, Terminating, and
// Orphaned (set as the claim is deleted while the bucket still holds objects).
//
// Deleting it removes the broker's IAM bindings from the bucket and deletes the
// bucket only if it is empty; a bucket with objects is kept, labelled
// idp-orphaned=true, and no tenant can reach it any more.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=bclaim
// +kubebuilder:printcolumn:name="Bucket",type=string,JSONPath=`.status.bucketName`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 18",message="name must be at most 18 characters"
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="name must be a DNS-1123 label"
type BucketClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec BucketClaimSpec `json:"spec,omitempty"`
	// +optional
	Status BucketClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type BucketClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BucketClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BucketClaim{}, &BucketClaimList{})
}
