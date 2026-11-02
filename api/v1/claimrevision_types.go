package v1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClaimRevisionSpec defines the desired state of ClaimRevision.
type ClaimRevisionSpec struct {
	// Version is the version of the Helm chart.
	// +kubebuilder:validation:MinLength=1
	// +required
	Version string `json:"version"`

	// Values is the values of the Helm chart.
	// +optional
	Values apiextensionsv1.JSON `json:"values"`

	// OCIUrl is the OCI repository URL where the service bundle is stored.
	// TODO: This field will be changed/removed in the future to reference a Flux OCIRepository resource instead of a raw URL.
	// +kubebuilder:validation:MinLength=1
	// +required
	OCIUrl string `json:"ociUrl"`

	// ApprovedAt is the timestamp when the revision was approved.
	// The newest approved revision is the one that will be used for deployment.
	// The revision
	// +optional
	ApprovedAt *metav1.Time `json:"approvedAt,omitempty"`
}

// ClaimRevisionStatus defines the observed state of ClaimRevision
type ClaimRevisionStatus struct {
}

// +genclient
//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Approved",type="date",JSONPath=".spec.approvedAt",description=""
//+kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.ociUrl`
//+kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""

// ClaimRevision is the Schema for the bundlesources API.
type ClaimRevision struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClaimRevisionSpec   `json:"spec,omitempty"`
	Status ClaimRevisionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClaimRevisionList contains a list of ClaimRevision.
type ClaimRevisionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClaimRevision `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClaimRevision{}, &ClaimRevisionList{})
}
