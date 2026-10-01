package postgresql

import (
	"errors"
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCreationErrorsDoNotExposeServerDetails(t *testing.T) {
	for _, test := range []struct {
		code string
		want error
	}{
		{"42710", application.ErrResourceConflict},
		{"42P04", application.ErrResourceConflict},
		{"23505", application.ErrResourceConflict},
		{"42501", application.ErrResourceUnavailable},
		{"25006", application.ErrResourceUnavailable},
		{"57014", application.ErrResourceUncertain},
		{"XX000", application.ErrResourceUncertain},
	} {
		actual := creationError(&pgconn.PgError{Code: test.code, Message: "unsafe SQL and credential detail"})
		if !errors.Is(actual, test.want) {
			t.Fatalf("SQLSTATE %s 分类错误", test.code)
		}
		if actual.Error() != test.want.Error() {
			t.Fatal("后端错误携带原始响应")
		}
	}
	if !errors.Is(creationError(errors.New("connection lost after send")), application.ErrResourceUncertain) {
		t.Fatal("未知网络结果不允许自动重试创建")
	}
}

func TestRolledBackErrorsAllowRetryExceptNameConflict(t *testing.T) {
	if !errors.Is(rolledBackError(&pgconn.PgError{Code: "42710"}), application.ErrResourceConflict) {
		t.Fatal("同名角色必须保留为冲突")
	}
	for _, err := range []error{&pgconn.PgError{Code: "57014"}, &pgconn.PgError{Code: "XX000"}, errors.New("connection lost after send")} {
		if !errors.Is(rolledBackError(err), application.ErrResourceUnavailable) {
			t.Fatalf("未提交事务的失败 %v 不应升级为不确定结果", err)
		}
	}
}
