package credential_test

import (
	"testing"

	credential "git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
)

func TestPreparationResume(t *testing.T) {
	for _, test := range []struct {
		name            string
		phase           credential.Phase
		version         int64
		continueAllowed bool
		result          credential.Phase
	}{
		{"尚未创建", credential.Pinned, 0, true, credential.Pinned},
		{"依赖恢复", credential.Unavailable, 0, true, credential.Unavailable},
		{"中断创建", credential.Creating, 0, false, credential.Conflict},
		{"未确认冲突", credential.Conflict, 0, false, credential.Conflict},
		{"已确认后读取失败", credential.Unavailable, 1, true, credential.Unavailable},
		{"已确认后冲突重验", credential.Conflict, 1, true, credential.Conflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := credential.State{Phase: test.phase, Version: test.version, Message: "保留原诊断"}
			state, allowed := original.Resume()
			if allowed != test.continueAllowed || state.Phase != test.result || state.Version != original.Version {
				t.Fatal("恢复判定或确认版本发生变化")
			}
			if test.phase == credential.Conflict && state.Message != original.Message {
				t.Fatal("冲突重入应保留原诊断")
			}
		})
	}
}

func TestPreparationLocationAndConfirmation(t *testing.T) {
	location := credential.Location{Mount: "applications", Path: "database/uid"}
	state := credential.State{Location: &location, Phase: credential.Creating}
	if issue := state.CheckLocation(location); issue != nil {
		t.Fatal("固定位置不应被拒绝")
	}
	if issue := state.CheckLocation(credential.Location{Mount: "other", Path: location.Path}); issue == nil || issue.Phase != credential.Unavailable {
		t.Fatal("配置变化必须停止，不迁移已固定位置")
	}
	for _, version := range []int64{0, -1, 2} {
		result, issue := state.Created(version)
		if issue == nil || issue.Phase != credential.Conflict || result.Confirmed() {
			t.Fatal("错误版本不得确认创建")
		}
	}
	result, issue := state.Created(1)
	if issue != nil || !result.Confirmed() || result.Phase != credential.Prepared || result.Location != state.Location {
		t.Fatal("首次写入回读应确认并保留位置")
	}
	if state.Version != 0 {
		t.Fatal("领域判定不得修改调用方的旧状态")
	}
}
