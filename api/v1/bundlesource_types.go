package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CustomResourceDefinitionSourceSpec defines the desired state of CustomResourceDefinitionSource.
type CustomResourceDefinitionSourceSpec struct {
	// Reference is a reference to the source of the bundle.
	Reference SourceReference `json:"reference"`
}

// +kubebuilder:object:root=true

// CustomResourceDefinitionSource is the Schema for the bundlesources API.
type CustomResourceDefinitionSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec CustomResourceDefinitionSourceSpec `json:"spec,omitempty"`
}

type SourceReference struct {
	// APIVersion of the referent.
	// +kubebuilder:validation:Enum=source.toolkit.fluxcd.io/v1
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`

	// Kind of the referent.
	// +kubebuilder:validation:Enum=OCIRepository
	// +required
	Kind string `json:"kind"`

	// Name of the referent.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// +kubebuilder:object:root=true

// CustomResourceDefinitionSourceList contains a list of CustomResourceDefinitionSource.
type CustomResourceDefinitionSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CustomResourceDefinitionSource `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CustomResourceDefinitionSource{}, &CustomResourceDefinitionSourceList{})
}
