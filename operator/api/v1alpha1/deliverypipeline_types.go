package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeliveryPipeline phases.
const (
	DeliveryPhasePending  = "PENDING"
	DeliveryPhaseBuilding = "BUILDING"
	DeliveryPhaseReady    = "READY"
	DeliveryPhaseFailed   = "FAILED"
)

type DeliveryPipelineSpec struct {
	// Workload is the name of a WorkloadIdentity in the same namespace. The
	// pipeline deploys as that workload (ServiceAccount and pod label).
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Workload string `json:"workload"`
	// Repo is the GitHub repository holding the Kustomize base, in the form
	// https://github.com/<org>/<repo> (no trailing slash, no .git). The
	// controller only accepts organisations on its allowlist
	// (DELIVERY_ALLOWED_REPO_ORGS).
	// +kubebuilder:validation:MaxLength=200
	// +kubebuilder:validation:Pattern=`^https://github\.com/[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`
	Repo string `json:"repo"`
	// Path is the Kustomize directory inside the repository: relative, no "..".
	// +kubebuilder:default="deploy/base"
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`
	// +kubebuilder:validation:XValidation:rule="!self.contains('..') && !self.contains('//') && !self.endsWith('/')",message="path must be relative, without '..', '//' or a trailing '/'"
	// +optional
	Path string `json:"path,omitempty"`
}

type DeliveryPipelineStatus struct {
	// +kubebuilder:validation:Enum=PENDING;BUILDING;READY;FAILED
	// +optional
	Phase string `json:"phase,omitempty"`
	// Message is short and sanitised: never a raw Gate response.
	// +kubebuilder:validation:MaxLength=300
	// +optional
	Message string `json:"message,omitempty"`
	// ApplicationName is the Spinnaker application, <namespace>-<workload>.
	// +optional
	ApplicationName string `json:"applicationName,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// DeliveryPipeline asks the delivery controller (idp-delivery, a separate
// Deployment, the only caller of Spinnaker's Gate; ADR 0011) for a Spinnaker
// application <namespace>-<workload> with one pipeline "deploy": bake the
// Kustomize base of the repository at a given commit, then deploy the result
// into the namespace through the Clouddriver account of the same name. The
// pipeline JSON comes from a template inside the controller; tenants never edit
// it. The object must be named after its workload (one pipeline per workload).
//
// Written by portal-api only (admission policy idp-delivery-scope). Deleting it
// does NOT delete the Spinnaker application or pipeline (follow-up).
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=dpipe
// +kubebuilder:printcolumn:name="Workload",type=string,JSONPath=`.spec.workload`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Application",type=string,JSONPath=`.status.applicationName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name == self.spec.workload",message="name must equal spec.workload"
// +kubebuilder:validation:XValidation:rule="self.spec.workload == oldSelf.spec.workload",message="spec.workload is immutable"
type DeliveryPipeline struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DeliveryPipelineSpec `json:"spec"`
	// +optional
	Status DeliveryPipelineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DeliveryPipelineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DeliveryPipeline `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DeliveryPipeline{}, &DeliveryPipelineList{})
}
