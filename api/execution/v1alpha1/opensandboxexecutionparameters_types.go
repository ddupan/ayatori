package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type NamespacedKeyReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Key       string `json:"key"`
}

type OpenSandboxRequestMapping string

const (
	OpenSandboxRequestMappingAdmissionOnly OpenSandboxRequestMapping = "AdmissionOnly"
	OpenSandboxRequestMappingNative        OpenSandboxRequestMapping = "Native"
)

// +kubebuilder:validation:XValidation:rule="self.allowInsecureHTTP || self.endpoint.startsWith('https://')",message="endpoint must use HTTPS unless allowInsecureHTTP is true"
type OpenSandboxExecutionParametersSpec struct {
	// +kubebuilder:validation:MinLength=1
	Endpoint        string                 `json:"endpoint"`
	APIKeySecretRef NamespacedKeyReference `json:"apiKeySecretRef"`
	// +optional
	PoolRef string `json:"poolRef,omitempty"`
	// +kubebuilder:validation:Enum=AdmissionOnly;Native
	// +kubebuilder:default=AdmissionOnly
	// +optional
	RequestMapping OpenSandboxRequestMapping `json:"requestMapping,omitempty"`
	// +optional
	AllowSecretEnv bool `json:"allowSecretEnv,omitempty"`
	// +optional
	AllowImageAuth bool `json:"allowImageAuth,omitempty"`
	// AllowInsecureHTTP is intended for isolated development environments only.
	// +optional
	AllowInsecureHTTP bool `json:"allowInsecureHTTP,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
type OpenSandboxExecutionParameters struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              OpenSandboxExecutionParametersSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type OpenSandboxExecutionParametersList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []OpenSandboxExecutionParameters `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &OpenSandboxExecutionParameters{}, &OpenSandboxExecutionParametersList{})
		return nil
	})
}
