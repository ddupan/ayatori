package postgresql

import (
	"errors"
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/database"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCreationErrorsDoNotExposeServerDetails(t *testing.T) {
	for _, test := range []struct {
		code string
		want error
	}{
		{"42710", database.ErrConflict},
		{"42P04", database.ErrConflict},
		{"23505", database.ErrConflict},
		{"42501", database.ErrUnavailable},
		{"25006", database.ErrUnavailable},
		{"57014", database.ErrUncertain},
		{"XX000", database.ErrUncertain},
	} {
		actual := creationError(&pgconn.PgError{Code: test.code, Message: "unsafe SQL and credential detail"})
		if !errors.Is(actual, test.want) {
			t.Fatalf("SQLSTATE %s 分类错误", test.code)
		}
		if actual.Error() != test.want.Error() {
			t.Fatal("后端错误携带原始响应")
		}
	}
	if !errors.Is(creationError(errors.New("connection lost after send")), database.ErrUncertain) {
		t.Fatal("未知网络结果不允许自动重试创建")
	}
}

func TestRolledBackErrorsAllowRetryExceptNameConflict(t *testing.T) {
	if !errors.Is(rolledBackError(&pgconn.PgError{Code: "42710"}), database.ErrConflict) {
		t.Fatal("同名角色必须保留为冲突")
	}
	for _, err := range []error{&pgconn.PgError{Code: "57014"}, &pgconn.PgError{Code: "XX000"}, errors.New("connection lost after send")} {
		if !errors.Is(rolledBackError(err), database.ErrUnavailable) {
			t.Fatalf("未提交事务的失败 %v 不应升级为不确定结果", err)
		}
	}
}
