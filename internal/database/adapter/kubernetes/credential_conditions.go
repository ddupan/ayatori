package kubernetes

import credentialdomain "git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"

const credentialsReadyCondition = "CredentialsReady"

// 持久化 API 的 Reason 保持兼容，领域内部只使用准备阶段。
var credentialReasons = map[credentialdomain.Phase]string{
	credentialdomain.Pending:       "PreparationPending",
	credentialdomain.Pinned:        "LocationPinned",
	credentialdomain.Creating:      "CreationStarted",
	credentialdomain.Prepared:      "CredentialPrepared",
	credentialdomain.Conflict:      "Conflict",
	credentialdomain.Unavailable:   "DependencyUnavailable",
	credentialdomain.Stopped:       "PreparationStopped",
	credentialdomain.InvalidTarget: "InvalidTarget",
}

func credentialReason(phase credentialdomain.Phase) string { return credentialReasons[phase] }

func credentialPhase(reason string) credentialdomain.Phase {
	for phase, value := range credentialReasons {
		if value == reason {
			return phase
		}
	}
	return credentialdomain.Pending
}
