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

	// Provides lists the DependencyGroups whose CRDs the chart of this source ships. It grants the
	// chart write access to those CRDs and lets Helm install them; it says nothing about who operates
	// them, that is Manages. Exactly one source may provide a group, otherwise two charts install the
	// same CRDs over each other. An operator that bundles its CRDs provides and manages the group,
	// while an operator whose CRDs come in a chart of their own only manages it.
	// +kubebuilder:validation:XValidation:rule="self.all(x, !has(x.dependencyGroup.as))",message="as must not be set on provides, the CRDs of a group are the same for every consumer"
	// +optional
	Provides []DependencyReference `json:"provides,omitempty"`

	// Manages declares that this source deploys the operator managing the CRDs of a DependencyGroup.
	// The instance of a managing source is the provider consumers are wired to: it is harnessed to the
	// group's scope and granted access to the namespaces that require the group.
	// A single group can be managed: the harness proxy scopes a cluster-scoped list to one label, and
	// a label selector cannot express "carries either label".
	// +kubebuilder:validation:MaxItems=1
	// +optional
	Manages []DependencyReference `json:"manages,omitempty"`

	// Requires lists the DependencyGroups that instances of this source consume.
	// +optional
	Requires []DependencyReference `json:"requires,omitempty"`
}

// DependencyReference references a dependency. Exactly one referent has to be set.
// +kubebuilder:validation:XValidation:rule="has(self.dependencyGroup)",message="dependencyGroup must be set"
type DependencyReference struct {
	// DependencyGroup references a DependencyGroup.
	// +optional
	DependencyGroup *DependencyGroupReference `json:"dependencyGroup,omitempty"`
}

// DependencyGroupReference references a DependencyGroup and names the scope it is used under.
type DependencyGroupReference struct {
	// Name of the DependencyGroup.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// As overrides the name the scope label is built from, so that a second deployment of the same
	// operator can serve a separate set of consumers. Both the managing operator and its consumers
	// have to set the same alias to end up in the same scope.
	// +optional
	As string `json:"as,omitempty"`
}

// ScopeName returns the name the scope label of the referenced group is built from.
func (r DependencyGroupReference) ScopeName() string {
	if r.As != "" {
		return r.As
	}
	return r.Name
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

// +genclient
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
