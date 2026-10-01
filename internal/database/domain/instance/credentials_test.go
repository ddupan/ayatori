package instance_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

func TestManagementCredentialsRejectEmptyValuesAndRedact(t *testing.T) {
	for _, values := range [][2]string{{"", "test-password"}, {"test-user", ""}, {"", ""}} {
		if _, err := instance.NewManagementCredentials(values[0], values[1]); err != instance.ErrCredentialsInvalid {
			t.Fatal("empty credential was accepted")
		}
	}
	const canary = "SECRET-CANARY-never-log-this"
	credentials, err := instance.NewManagementCredentials(canary, canary)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Username() != canary || credentials.Password() != canary {
		t.Fatal("explicit credential access changed values")
	}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{string(encoded), fmt.Sprintf("%v %+v %#v", credentials, credentials, credentials)} {
		if strings.Contains(output, canary) {
			t.Fatal("formatting leaked credential data")
		}
	}
}

func TestManagementFactsRequireNativeNonSuperuser(t *testing.T) {
	native := instance.ManagementFacts{Version: "17", CreateRole: true, CreateDatabase: true}
	if checks := native.Checks(); checks != (instance.ManagementChecks{
		Connection: instance.CheckPassed, Metadata: instance.CheckPassed, Roles: instance.CheckPassed,
		Databases: instance.CheckPassed, Grants: instance.CheckPassed, Extensions: instance.CheckPassed,
	}) {
		t.Fatalf("原生管理账号应通过全部检查：%+v", checks)
	}
	for name, facts := range map[string]instance.ManagementFacts{
		"superuser":    {CreateRole: true, CreateDatabase: true, Superuser: true},
		"无 CREATEROLE": {CreateDatabase: true},
		"无 CREATEDB":   {CreateRole: true},
	} {
		checks := facts.Checks()
		if checks.Grants != instance.CheckInsufficientPrivileges {
			t.Fatalf("%s 不应满足授权能力", name)
		}
	}
	readOnly := native
	readOnly.ReadOnly = true
	if readOnly.Checks().Databases != instance.CheckUnavailable {
		t.Fatal("只读实例不能建库")
	}
}
