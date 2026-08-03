package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OperatorHarnessSpec defines the desired state of OperatorHarness.
type OperatorHarnessSpec struct {
	// ScopeToLabel is the label that needs to be present on a resource for the harnessed operator to manage it.
	// It is used to scope cluster-wide permissions: if the harnessed operator does a cluster-scoped list or
	// watch through the harness proxy, it only sees resources carrying this label.
	// +kubebuilder:validation:MinLength=1
	// +required
	ScopeToLabel string `json:"scopeToLabel"`

	// Operator describes the harnessed operator's workload.
	// +required
	Operator OperatorHarnessOperator `json:"operator"`
}

// OperatorHarnessOperator describes where the harnessed operator runs and how it is harnessed.
type OperatorHarnessOperator struct {
	// Namespace is the namespace the harnessed operator runs in.
	// +kubebuilder:validation:MinLength=1
	// +required
	Namespace string `json:"namespace"`

	// ServiceAccounts are the service accounts, in Namespace, the harnessed operator's pods run as.
	// Only pods running as one of these service accounts are harnessed.
	// +kubebuilder:validation:MinItems=1
	// +required
	ServiceAccounts []string `json:"serviceAccounts"`

	// InjectProxyConfiguration defines if the harnessed operator should have the harness proxy injected.
	// If enabled, the harness copies the proxy's CA certificate to Namespace and registers a mutating
	// webhook that points the operator's pods at the harness proxy instead of the Kubernetes API server.
	// +optional
	InjectProxyConfiguration bool `json:"injectProxyConfiguration,omitempty"`
}

// OperatorHarnessStatus defines the observed state of OperatorHarness.
type OperatorHarnessStatus struct {
	// Conditions holds the conditions for the OperatorHarness.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +genclient
// +genclient:nonNamespaced
//+kubebuilder:object:root=true
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.operator.namespace`
//+kubebuilder:printcolumn:name="ScopeToLabel",type=string,JSONPath=`.spec.scopeToLabel`
//+kubebuilder:printcolumn:name="ProxyInjection",type=boolean,JSONPath=`.spec.operator.injectProxyConfiguration`
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""
//+kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
//+kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].message",description=""

// OperatorHarness is the Schema for the operatorharnesses API.
// It harnesses an operator that would otherwise require cluster-wide permissions by scoping its
// access to resources carrying the label in `.spec.scopeToLabel`.
type OperatorHarness struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OperatorHarnessSpec   `json:"spec,omitempty"`
	Status OperatorHarnessStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OperatorHarnessList contains a list of OperatorHarness.
type OperatorHarnessList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OperatorHarness `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OperatorHarness{}, &OperatorHarnessList{})
}
