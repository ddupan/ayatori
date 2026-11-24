package controller

import (
	"testing"

	databasev1alpha1 "go.ddupan.top/ayatori/api/database/v1alpha1"
	"go.ddupan.top/ayatori/internal/database/domain/database"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// 阶段只来自显式 status 字段；Condition Reason 只是呈现，改写它不能改变 controller 的判断。
func TestSnapshotReadsPhasesFromStatusFields(t *testing.T) {
	object := &databasev1alpha1.PostgreSQLDatabase{}
	object.Status.CredentialPhase = string(database.CredentialPrepared)
	object.Status.ResourcePhase = string(database.ResourceAvailable)
	object.Status.Conditions = []metav1.Condition{
		{Type: credentialsReadyCondition, Reason: "Conflict", Message: "凭据诊断"},
		{Type: resourcesReadyCondition, Reason: "Conflict", Message: "资源诊断"},
	}
	state := snapshot(object)
	if state.Credential.Phase != database.CredentialPrepared || state.Resources.Phase != database.ResourceAvailable {
		t.Fatalf("阶段被 Condition Reason 影响：%v / %v", state.Credential.Phase, state.Resources.Phase)
	}
	if state.Credential.Message != "凭据诊断" || state.Resources.Message != "资源诊断" {
		t.Fatal("诊断消息应保留以便冲突沿用首次原因")
	}
	empty := snapshot(&databasev1alpha1.PostgreSQLDatabase{})
	if empty.Credential.Phase != database.CredentialPending || empty.Resources.Phase != database.ResourcePending {
		t.Fatal("未写入时应从 Pending 开始")
	}
}
