package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type KubernetesSchedulingParameters struct {
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

type KubernetesExecutionParametersSpec struct {
	// +kubebuilder:validation:MinLength=1
	ServiceAccountName string `json:"serviceAccountName"`
	// +optional
	RuntimeClassName string `json:"runtimeClassName,omitempty"`
	// +optional
	Scheduling KubernetesSchedulingParameters `json:"scheduling,omitempty"`
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +kubebuilder:default=IfNotPresent
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
type KubernetesExecutionParameters struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              KubernetesExecutionParametersSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type KubernetesExecutionParametersList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []KubernetesExecutionParameters `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &KubernetesExecutionParameters{}, &KubernetesExecutionParametersList{})
		return nil
	})
}
