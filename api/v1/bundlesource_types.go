package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BundleSourceSpec defines the desired state of BundleSource.
type BundleSourceSpec struct {
	// OCIURI is the URI of the OCI registry where the bundles are stored.
	// +required
	OCIURI string `json:"ociURI"`
	// MajorVersions defines the major versions of the bundles that should be made available.
	// +required
	MajorVersions []uint `json:"majorVersions"`
}

// +kubebuilder:object:root=true

// BundleSource is the Schema for the bundlesources API.
type BundleSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec BundleSourceSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// BundleSourceList contains a list of BundleSource.
type BundleSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BundleSource `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BundleSource{}, &BundleSourceList{})
}
