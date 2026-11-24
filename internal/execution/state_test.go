package execution

import (
	"testing"
	"time"

	executionv1alpha1 "go.ddupan.top/ayatori/api/execution/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStateOf(t *testing.T) {
	now := metav1.NewTime(time.Now())
	tests := []struct {
		name string
		job  executionv1alpha1.Job
		want JobState
	}{
		{name: "resolving", want: JobStateResolving},
		{name: "scheduling", job: jobWithConditions(newCondition(executionv1alpha1.JobConditionAccepted, metav1.ConditionTrue, "Valid")), want: JobStateScheduling},
		{name: "starting", job: jobWithConditions(newCondition(executionv1alpha1.JobConditionScheduled, metav1.ConditionTrue, "BackendCreated")), want: JobStateStarting},
		{name: "running", job: executionv1alpha1.Job{Status: executionv1alpha1.JobStatus{StartTime: &now}}, want: JobStateRunning},
		{name: "result unknown", job: jobWithConditions(newCondition(executionv1alpha1.JobConditionSucceeded, metav1.ConditionUnknown, "ResultUnknown")), want: JobStateResultUnknown},
		{name: "cancelling", job: executionv1alpha1.Job{Spec: executionv1alpha1.JobSpec{DesiredState: executionv1alpha1.JobDesiredStateCancelled}}, want: JobStateCancelling},
		{name: "succeeded", job: jobWithConditions(newCondition(executionv1alpha1.JobConditionSucceeded, metav1.ConditionTrue, "Completed")), want: JobStateSucceeded},
		{name: "failed", job: jobWithConditions(newCondition(executionv1alpha1.JobConditionSucceeded, metav1.ConditionFalse, "ExitCode")), want: JobStateFailed},
		{name: "cancelled", job: jobWithConditions(newCondition(executionv1alpha1.JobConditionSucceeded, metav1.ConditionFalse, "Cancelled")), want: JobStateCancelled},
		{name: "terminal beats desired cancellation", job: func() executionv1alpha1.Job {
			j := jobWithConditions(newCondition(executionv1alpha1.JobConditionSucceeded, metav1.ConditionTrue, "Completed"))
			j.Spec.DesiredState = executionv1alpha1.JobDesiredStateCancelled
			return j
		}(), want: JobStateSucceeded},
		{name: "deleting beats terminal", job: func() executionv1alpha1.Job {
			j := jobWithConditions(newCondition(executionv1alpha1.JobConditionSucceeded, metav1.ConditionTrue, "Completed"))
			j.DeletionTimestamp = &now
			return j
		}(), want: JobStateDeleting},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StateOf(&tt.job); got != tt.want {
				t.Fatalf("StateOf() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateStatus(t *testing.T) {
	start := metav1.NewTime(time.Unix(100, 0))
	finish := metav1.NewTime(time.Unix(200, 0))
	earlier := metav1.NewTime(time.Unix(50, 0))
	scheduled := newCondition(executionv1alpha1.JobConditionScheduled, metav1.ConditionTrue, "BackendCreated")
	succeeded := newCondition(executionv1alpha1.JobConditionSucceeded, metav1.ConditionTrue, "Completed")

	tests := []struct {
		name    string
		status  executionv1alpha1.JobStatus
		wantErr bool
	}{
		{name: "empty status"},
		{name: "running", status: executionv1alpha1.JobStatus{Conditions: []metav1.Condition{scheduled}, StartTime: &start}},
		{name: "completed", status: executionv1alpha1.JobStatus{Conditions: []metav1.Condition{scheduled, succeeded}, StartTime: &start, CompletionTime: &finish}},
		{name: "terminal without completion time", status: executionv1alpha1.JobStatus{Conditions: []metav1.Condition{succeeded}}, wantErr: true},
		{name: "completion without terminal", status: executionv1alpha1.JobStatus{CompletionTime: &finish}, wantErr: true},
		{name: "start without scheduling", status: executionv1alpha1.JobStatus{StartTime: &start}, wantErr: true},
		{name: "completion before start", status: executionv1alpha1.JobStatus{Conditions: []metav1.Condition{scheduled, succeeded}, StartTime: &start, CompletionTime: &earlier}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateStatus(&executionv1alpha1.Job{Status: tt.status})
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateStatus() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func newCondition(conditionType string, status metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{Type: conditionType, Status: status, Reason: reason}
}

func jobWithConditions(conditions ...metav1.Condition) executionv1alpha1.Job {
	return executionv1alpha1.Job{Status: executionv1alpha1.JobStatus{Conditions: conditions}}
}
