package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeliveryRun phases.
const (
	DeliveryRunPending   = "PENDING"
	DeliveryRunRunning   = "RUNNING"
	DeliveryRunSucceeded = "SUCCEEDED"
	DeliveryRunFailed    = "FAILED"
)

// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a DeliveryRun spec is immutable"
type DeliveryRunSpec struct {
	// Pipeline is the name of a DeliveryPipeline in the same namespace.
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Pipeline string `json:"pipeline"`
	// Ref is the full 40-hex git commit SHA to bake and deploy.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	Ref string `json:"ref"`
	// RequestedBy is the Keycloak subject that asked for the deploy, set by the
	// portal. Informational only: it grants nothing.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	RequestedBy string `json:"requestedBy,omitempty"`
}

type DeliveryRunStatus struct {
	// +kubebuilder:validation:Enum=PENDING;RUNNING;SUCCEEDED;FAILED
	// +optional
	Phase string `json:"phase,omitempty"`
	// ExecutionID is the Spinnaker pipeline execution.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	ExecutionID string `json:"executionId,omitempty"`
	// Message is short and sanitised: never a raw Gate response.
	// +kubebuilder:validation:MaxLength=300
	// +optional
	Message string `json:"message,omitempty"`
}

// DeliveryRun asks the delivery controller to run the pipeline of a
// DeliveryPipeline for one commit. It is a one-shot request: once the phase is
// SUCCEEDED or FAILED nothing more happens. Written by portal-api only.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=drun
// +kubebuilder:printcolumn:name="Pipeline",type=string,JSONPath=`.spec.pipeline`
// +kubebuilder:printcolumn:name="Ref",type=string,JSONPath=`.spec.ref`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type DeliveryRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DeliveryRunSpec `json:"spec"`
	// +optional
	Status DeliveryRunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DeliveryRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DeliveryRun `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DeliveryRun{}, &DeliveryRunList{})
}
