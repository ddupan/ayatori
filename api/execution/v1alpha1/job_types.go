package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

const (
	JobConditionAccepted  = "Accepted"
	JobConditionScheduled = "Scheduled"
	JobConditionSucceeded = "Succeeded"
)

type JobDesiredState string

const (
	JobDesiredStateRunning   JobDesiredState = "Running"
	JobDesiredStateCancelled JobDesiredState = "Cancelled"
)

type TaskSpec struct {
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	// +optional
	Command []string `json:"command,omitempty"`
	// +optional
	Args []string `json:"args,omitempty"`
	// +optional
	WorkingDir string `json:"workingDir,omitempty"`
	// +listType=map
	// +listMapKey=name
	// +optional
	Env []EnvVar `json:"env,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.value) != has(self.valueFrom)",message="exactly one of value or valueFrom must be set"
type EnvVar struct {
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	Name string `json:"name"`
	// +optional
	Value *string `json:"value,omitempty"`
	// +optional
	ValueFrom *EnvVarSource `json:"valueFrom,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.secretKeyRef) != has(self.configMapKeyRef)",message="exactly one key reference must be set"
type EnvVarSource struct {
	// +optional
	SecretKeyRef *corev1.SecretKeySelector `json:"secretKeyRef,omitempty"`
	// +optional
	ConfigMapKeyRef *corev1.ConfigMapKeySelector `json:"configMapKeyRef,omitempty"`
}

type ResourceValues struct {
	// +optional
	CPU *resource.Quantity `json:"cpu,omitempty"`
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`
}

type ExecutionResourceRequirements struct {
	// +optional
	Requests ResourceValues `json:"requests,omitempty"`
	// +optional
	Limits ResourceValues `json:"limits,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.task == oldSelf.task",message="task is immutable"
// +kubebuilder:validation:XValidation:rule="self.resources == oldSelf.resources",message="resources are immutable"
// +kubebuilder:validation:XValidation:rule="has(self.activeDeadlineSeconds) == has(oldSelf.activeDeadlineSeconds) && (!has(self.activeDeadlineSeconds) || self.activeDeadlineSeconds == oldSelf.activeDeadlineSeconds)",message="activeDeadlineSeconds is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.jobClassName) == has(oldSelf.jobClassName) && (!has(self.jobClassName) || self.jobClassName == oldSelf.jobClassName)",message="jobClassName is immutable"
// +kubebuilder:validation:XValidation:rule="oldSelf.desiredState == self.desiredState || (oldSelf.desiredState == 'Running' && self.desiredState == 'Cancelled')",message="desiredState may only transition from Running to Cancelled"
type JobSpec struct {
	// +optional
	JobClassName string   `json:"jobClassName,omitempty"`
	Task         TaskSpec `json:"task"`
	// +optional
	Resources ExecutionResourceRequirements `json:"resources,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +optional
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
	// +kubebuilder:validation:Enum=Running;Cancelled
	// +kubebuilder:default=Running
	// +optional
	DesiredState JobDesiredState `json:"desiredState,omitempty"`
}

type ParametersReference struct {
	Group string `json:"group"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	// +optional
	UID types.UID `json:"uid,omitempty"`
}

type ResolvedJobClassReference struct {
	Name           string              `json:"name"`
	UID            types.UID           `json:"uid"`
	ControllerName string              `json:"controllerName"`
	ParametersRef  ParametersReference `json:"parametersRef"`
}

type ExecutionReference struct {
	// +kubebuilder:validation:MinLength=1
	Type string `json:"type"`
	// +kubebuilder:validation:MinLength=1
	ID string `json:"id"`
}

type ExecutionStatus struct {
	Adapter string `json:"adapter"`
	// +listType=map
	// +listMapKey=type
	// +optional
	References []ExecutionReference `json:"references,omitempty"`
}

type JobResult struct {
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
}

type JobStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ResolvedJobClass *ResolvedJobClassReference `json:"resolvedJobClass,omitempty"`
	// +optional
	EffectiveResources ExecutionResourceRequirements `json:"effectiveResources,omitempty"`
	// +optional
	Execution *ExecutionStatus `json:"execution,omitempty"`
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// +optional
	Result *JobResult `json:"result,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=='Accepted')].status`
// +kubebuilder:printcolumn:name="Scheduled",type=string,JSONPath=`.status.conditions[?(@.type=='Scheduled')].status`
// +kubebuilder:printcolumn:name="Succeeded",type=string,JSONPath=`.status.conditions[?(@.type=='Succeeded')].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Job struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              JobSpec `json:"spec"`
	// +optional
	Status JobStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true
type JobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Job `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Job{}, &JobList{})
		return nil
	})
}
