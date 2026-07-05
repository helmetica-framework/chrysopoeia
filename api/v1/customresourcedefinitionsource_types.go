package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CustomResourceDefinitionSourceSpec defines the desired state of CustomResourceDefinitionSource.
type CustomResourceDefinitionSourceSpec struct {
	// Reference is a reference to the source of the bundle.
	Reference SourceReference `json:"reference"`

	// VersionDiscovery defines how to discover the version of the source.
	// +optional
	VersionDiscovery VersionDiscovery `json:"versionDiscovery,omitempty"`
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

type VersionDiscovery struct {
	// Reference is a reference to the discovery source of the version.
	Reference DiscoveryReference `json:"reference"`
}

type DiscoveryReference struct {
	// APIVersion of the referent.
	// +kubebuilder:validation:Enum=image.toolkit.fluxcd.io/v1
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`

	// Kind of the referent.
	// +kubebuilder:validation:Enum=ImageRepository
	// +required
	Kind string `json:"kind"`

	// Name of the referent.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// CustomResourceDefinitionSourceStatus defines the observed state of CustomResourceDefinitionSource
type CustomResourceDefinitionSourceStatus struct {
	// Conditions holds the conditions for the CustomResourceDefinitionSource.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// AppliedReferenceGeneration is the last applied generation of the referenced source.
	// +optional
	AppliedReferenceGeneration int64 `json:"appliedReferenceGeneration,omitempty"`

	// AppliedReferenceRevision is the last applied revision of the referenced source.
	// +optional
	AppliedReferenceRevision string `json:"appliedReferenceRevision,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// CustomResourceDefinitionSource is the Schema for the bundlesources API.
type CustomResourceDefinitionSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CustomResourceDefinitionSourceSpec   `json:"spec,omitempty"`
	Status CustomResourceDefinitionSourceStatus `json:"status,omitempty"`
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
