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
