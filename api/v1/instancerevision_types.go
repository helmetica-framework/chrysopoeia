package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// InstanceRevisionSpec defines the desired state of InstanceRevision.
type InstanceRevisionSpec struct {
	// Version is the version of the Helm chart.
	// +kubebuilder:validation:MinLength=1
	// +required
	Version string `json:"version"`

	// Values is the values of the Helm chart.
	Values runtime.RawExtension `json:"values"`

	// OCIUrl is the OCI repository URL where the service bundle is stored.
	// TODO: This field will be changed/removed in the future to reference a Flux OCIRepository resource instead of a raw URL.
	// +kubebuilder:validation:MinLength=1
	// +required
	OCIUrl string `json:"ociUrl"`
}

// InstanceRevisionStatus defines the observed state of InstanceRevision
type InstanceRevisionStatus struct {
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.ociUrl`
//+kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""

// InstanceRevision is the Schema for the bundlesources API.
type InstanceRevision struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InstanceRevisionSpec   `json:"spec,omitempty"`
	Status InstanceRevisionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// InstanceRevisionList contains a list of InstanceRevision.
type InstanceRevisionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []InstanceRevision `json:"items"`
}

func init() {
	SchemeBuilder.Register(&InstanceRevision{}, &InstanceRevisionList{})
}
