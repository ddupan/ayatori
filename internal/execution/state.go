package execution

import (
	"fmt"

	executionv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/execution/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type JobState string

const (
	JobStateResolving     JobState = "Resolving"
	JobStateScheduling    JobState = "Scheduling"
	JobStateStarting      JobState = "Starting"
	JobStateRunning       JobState = "Running"
	JobStateCancelling    JobState = "Cancelling"
	JobStateResultUnknown JobState = "ResultUnknown"
	JobStateSucceeded     JobState = "Succeeded"
	JobStateFailed        JobState = "Failed"
	JobStateCancelled     JobState = "Cancelled"
	JobStateDeleting      JobState = "Deleting"
)

// StateOf derives a presentation state from durable API facts. The derived
// state is deliberately not persisted as a second source of truth.
func StateOf(job *executionv1alpha1.Job) JobState {
	if !job.DeletionTimestamp.IsZero() {
		return JobStateDeleting
	}

	succeeded := condition(job.Status.Conditions, executionv1alpha1.JobConditionSucceeded)
	if succeeded != nil {
		switch succeeded.Status {
		case metav1.ConditionTrue:
			return JobStateSucceeded
		case metav1.ConditionFalse:
			if succeeded.Reason == "Cancelled" {
				return JobStateCancelled
			}
			return JobStateFailed
		}
	}

	if job.Spec.DesiredState == executionv1alpha1.JobDesiredStateCancelled {
		return JobStateCancelling
	}
	if succeeded != nil && succeeded.Status == metav1.ConditionUnknown && succeeded.Reason == "ResultUnknown" {
		return JobStateResultUnknown
	}
	if job.Status.StartTime != nil {
		return JobStateRunning
	}
	if conditionTrue(job.Status.Conditions, executionv1alpha1.JobConditionScheduled) {
		return JobStateStarting
	}
	if conditionTrue(job.Status.Conditions, executionv1alpha1.JobConditionAccepted) {
		return JobStateScheduling
	}
	return JobStateResolving
}

// ValidateStatus checks invariants that every adapter must preserve.
func ValidateStatus(job *executionv1alpha1.Job) error {
	succeeded := condition(job.Status.Conditions, executionv1alpha1.JobConditionSucceeded)
	terminal := succeeded != nil && (succeeded.Status == metav1.ConditionTrue || succeeded.Status == metav1.ConditionFalse)
	if terminal && job.Status.CompletionTime == nil {
		return fmt.Errorf("terminal job must have completionTime")
	}
	if job.Status.CompletionTime != nil && !terminal {
		return fmt.Errorf("completionTime requires a terminal Succeeded condition")
	}
	if job.Status.StartTime != nil && !conditionTrue(job.Status.Conditions, executionv1alpha1.JobConditionScheduled) {
		return fmt.Errorf("startTime requires Scheduled=True")
	}
	if job.Status.StartTime != nil && job.Status.CompletionTime != nil && job.Status.CompletionTime.Before(job.Status.StartTime) {
		return fmt.Errorf("completionTime must not precede startTime")
	}
	return nil
}

func condition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func conditionTrue(conditions []metav1.Condition, conditionType string) bool {
	current := condition(conditions, conditionType)
	return current != nil && current.Status == metav1.ConditionTrue
}
