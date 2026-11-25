/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// TestSpec defines the desired state of Test
type TestSpec struct {
}

// TestStatus defines the observed state of Test.
type TestStatus struct {
	ControllerID string `json:"controllerId,omitzero"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +genclient
//+kubebuilder:printcolumn:name="ControllerID",type=string,JSONPath=`.status.controllerId`

// Test is the Schema for the tests API
type Test struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Test
	// +optional
	Spec TestSpec `json:"spec"`

	// status defines the observed state of Test
	// +optional
	Status TestStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// TestList contains a list of Test
type TestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Test `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Test{}, &TestList{})
		return nil
	})
}
