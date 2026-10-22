// Package v1alpha1 contains the v1alpha1 API of the idp.ithaca-idp.com group.
// +kubebuilder:object:generate=true
// +groupName=idp.ithaca-idp.com
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion = schema.GroupVersion{Group: "idp.ithaca-idp.com", Version: "v1alpha1"}

	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	AddToScheme = SchemeBuilder.AddToScheme
)
