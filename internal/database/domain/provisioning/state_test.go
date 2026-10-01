package provisioning

import "testing"

func TestResume(t *testing.T) {
	for _, test := range []struct {
		name   string
		state  State
		resume bool
	}{
		{"初次", State{}, true},
		{"角色创建不确定", State{Phase: CreatingRole}, false},
		{"库创建不确定", State{RoleOID: 11, Phase: CreatingDatabase}, false},
		{"已确认角色", State{RoleOID: 11, Phase: Pending}, true},
		{"已确认资源暂不可用", State{RoleOID: 11, DatabaseOID: 22, Phase: Unavailable}, true},
		{"冲突保持", State{RoleOID: 11, Phase: Conflict, Message: "首次诊断"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, resume := test.state.Resume()
			if resume != test.resume || state.RoleOID != test.state.RoleOID || state.DatabaseOID != test.state.DatabaseOID {
				t.Fatal("恢复决策丢失确认或错误放行")
			}
			if !resume && state.Phase != Conflict {
				t.Fatal("不确定创建未转冲突")
			}
			if test.state.Phase == Conflict && state != test.state {
				t.Fatal("冲突诊断被覆盖")
			}
		})
	}
}

func TestObservedIdentity(t *testing.T) {
	state := State{RoleOID: 11, DatabaseOID: 22}
	good := Observation{RoleOID: 11, DatabaseOID: 22, RoleSafe: true, OwnerOID: 11}
	if err := state.Check(good); err != nil {
		t.Fatal(err)
	}
	for _, observed := range []Observation{
		{}, {RoleOID: 12, DatabaseOID: 22, RoleSafe: true, OwnerOID: 12},
		{RoleOID: 11, DatabaseOID: 23, RoleSafe: true, OwnerOID: 11},
		{RoleOID: 11, DatabaseOID: 22, RoleSafe: false, OwnerOID: 11},
		{RoleOID: 11, DatabaseOID: 22, RoleSafe: true, OwnerOID: 99},
	} {
		if state.Check(observed) == nil {
			t.Fatal("对象身份或权限漂移被接受")
		}
	}
	if (State{}).Check(good) == nil {
		t.Fatal("未确认同名对象被认领")
	}
}
