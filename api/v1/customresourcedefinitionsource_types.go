package v1

import (
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CustomResourceDefinitionSourceSpec defines the desired state of CustomResourceDefinitionSource.
type CustomResourceDefinitionSourceSpec struct {
	// Reference is a reference to the source of the bundle.
	Reference SourceReference `json:"reference"`

	// CRDNames is the `.spec.names` field of the generated [apiextv1.CustomResourceDefinition].
	// If not set takes the name from the chart's metadata or falls back to the default `Instance` kind.
	// Please note that changing this field generates a new CRD and does not update the existing one.
	// The old CRD will be left in the cluster and must be removed manually.
	// +optional
	CRDNames apiextv1.CustomResourceDefinitionNames `json:"crdNames,omitempty"`

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
//+kubebuilder:printcolumn:name="SourceRef",type=string,JSONPath=`.spec.reference.name`
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""
//+kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
//+kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].message",description=""

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
