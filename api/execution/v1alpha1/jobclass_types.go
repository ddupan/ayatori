package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	JobClassConditionAccepted = "Accepted"
	JobClassConditionReady    = "Ready"
)

type ExecutionResourcePolicy struct {
	// +optional
	Defaults ExecutionResourceRequirements `json:"defaults,omitempty"`
	// +optional
	Minimum ExecutionResourceRequirements `json:"minimum,omitempty"`
	// +optional
	Maximum ExecutionResourceRequirements `json:"maximum,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.controllerName == oldSelf.controllerName",message="controllerName is immutable"
// +kubebuilder:validation:XValidation:rule="self.parametersRef == oldSelf.parametersRef",message="parametersRef is immutable"
type JobClassSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ControllerName string              `json:"controllerName"`
	ParametersRef  ParametersReference `json:"parametersRef"`
	// +optional
	AllowedNamespaces *metav1.LabelSelector `json:"allowedNamespaces,omitempty"`
	// +optional
	Resources ExecutionResourcePolicy `json:"resources,omitempty"`
}

type JobClassStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Controller",type=string,JSONPath=`.spec.controllerName`
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=='Accepted')].status`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type JobClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              JobClassSpec `json:"spec"`
	// +optional
	Status JobClassStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true
type JobClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []JobClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &JobClass{}, &JobClassList{})
		return nil
	})
}
