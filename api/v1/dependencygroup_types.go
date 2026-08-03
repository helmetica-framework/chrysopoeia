package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DependencyGroupSpec defines the desired state of DependencyGroup.
type DependencyGroupSpec struct {
	// CRDs are the CustomResourceDefinitions that belong to this group.
	// A CRD must not belong to more than one group, groups claiming an already claimed CRD are
	// rejected. The CRDs do not have to exist yet.
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	// +required
	CRDs []DependencyGroupCRD `json:"crds"`

	// HarnessRef references the harness of the operator managing the CRDs of this group.
	// +optional
	HarnessRef *HarnessReference `json:"harnessRef,omitempty"`
}

// DependencyGroupCRD identifies a CustomResourceDefinition belonging to a DependencyGroup.
type DependencyGroupCRD struct {
	// Name is the fully qualified name of the CRD, as written in its `.metadata.name`.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// HarnessReference references an OperatorHarness.
type HarnessReference struct {
	// Kind of the referent.
	// +kubebuilder:validation:Enum=OperatorHarness
	// +required
	Kind string `json:"kind"`

	// Name of the referent.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// DependencyGroupState is the state of a DependencyGroup's claim on its CRDs.
// +kubebuilder:validation:Enum=Accepted;Rejected
type DependencyGroupState string

const (
	// DependencyGroupAccepted means the group's claim on all of its CRDs is undisputed.
	DependencyGroupAccepted DependencyGroupState = "Accepted"
	// DependencyGroupRejected means at least one of the group's CRDs is claimed by another group.
	DependencyGroupRejected DependencyGroupState = "Rejected"
)

// DependencyGroupStatus defines the observed state of DependencyGroup.
type DependencyGroupStatus struct {
	// State is Accepted if the group holds the claim on all of its CRDs, and Rejected otherwise.
	// Only accepted groups may be referenced by a CustomResourceDefinitionSource.
	// +optional
	State DependencyGroupState `json:"state,omitempty"`

	// Conditions holds the conditions for the DependencyGroup. The Accepted condition carries the
	// reason a group was rejected.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +genclient
// +genclient:nonNamespaced
//+kubebuilder:object:root=true
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
//+kubebuilder:printcolumn:name="Harness",type=string,JSONPath=`.spec.harnessRef.name`
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""
//+kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type==\"Accepted\")].message",description=""

// DependencyGroup is the Schema for the dependencygroups API.
// It bundles the CustomResourceDefinitions of a service so that charts can depend on the group
// instead of on individual CRDs, and ties them to the harness of the managing operator.
type DependencyGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DependencyGroupSpec   `json:"spec,omitempty"`
	Status DependencyGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DependencyGroupList contains a list of DependencyGroup.
type DependencyGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DependencyGroup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DependencyGroup{}, &DependencyGroupList{})
}
