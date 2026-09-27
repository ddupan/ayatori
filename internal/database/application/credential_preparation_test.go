package application

import (
	"context"
	"errors"
	"testing"

	credentialdomain "git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/binding"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

const (
	preparationInstanceName = "credential-instance"
	preparationInstanceUID  = "credential-instance-uid"
)

func preparationRecord(t *testing.T) *CredentialRecord {
	t.Helper()
	tenant := binding.TenantIdentity{Namespace: bindingTestNamespace, Name: bindingTestName, UID: "credential-tenant-uid"}
	database := binding.Identity{Name: binding.DynamicDatabaseName(tenant.UID), UID: "credential-database-uid"}
	endpoint, err := instance.NewEndpoint(instance.EndpointValues{
		Host: "postgres.example", HostAddr: "192.0.2.1", Port: 5432, ManagementDatabase: "management", TLSMode: instance.TLSVerifyFull,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &CredentialRecord{
		Database: binding.Database{
			Identity: database, Instance: preparationInstanceName, InstanceUID: preparationInstanceUID, Name: bindingTestName, LoginRole: bindingTestName, Source: "Provision", Tenant: &tenant,
		},
		Tenant: &binding.Tenant{
			Identity: tenant, Phase: binding.Bound, Database: &database,
			Request: binding.Request{Provision: &binding.ProvisionRequest{Instance: preparationInstanceName}},
		},
		Instance:          &credentialdomain.Instance{Identity: binding.Identity{Name: preparationInstanceName, UID: preparationInstanceUID}, Ready: true, Endpoint: endpoint},
		DatabaseProtected: true, TenantProtected: true,
	}
}

type memoryCredentialResources struct {
	record     *CredentialRecord
	saveError  error
	checkError error
}

func (r *memoryCredentialResources) Load(context.Context, string) (*CredentialRecord, error) {
	copy := *r.record
	return &copy, nil
}

func (r *memoryCredentialResources) Save(_ context.Context, record *CredentialRecord, status credentialdomain.State) (*CredentialRecord, error) {
	if r.saveError != nil {
		return nil, r.saveError
	}
	copy := *record
	copy.Status = status
	r.record = &copy
	return &copy, nil
}

func (r *memoryCredentialResources) CheckCurrent(context.Context, *CredentialRecord) error {
	return r.checkError
}

type preparationStore struct {
	reads       int
	creates     int
	readError   error
	createError error
}

func (*preparationStore) ProvisionLocation(uid string) (credentialdomain.Location, error) {
	return credentialdomain.Location{Mount: "applications", Path: "database/" + uid}, nil
}

func (s *preparationStore) ReadCredential(context.Context, credentialdomain.Location, int64) (credentialdomain.ApplicationCredential, error) {
	s.reads++
	return credentialdomain.ApplicationCredential{}, s.readError
}

func (s *preparationStore) CreateCredential(context.Context, credentialdomain.Location, credentialdomain.ApplicationCredential) (int64, error) {
	s.creates++
	if s.createError != nil {
		return 0, s.createError
	}
	return 1, nil
}

func TestCredentialPreparationGates(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*CredentialRecord)
	}{
		{"导入不供应", func(r *CredentialRecord) { r.Database.Source = "Import" }},
		{"删除中的资源", func(r *CredentialRecord) { r.Database.Deleting = true }},
		{"已释放资源", func(r *CredentialRecord) { r.Database.Phase = "Released" }},
		{"缺少资源保护", func(r *CredentialRecord) { r.DatabaseProtected = false }},
		{"缺少申请保护", func(r *CredentialRecord) { r.TenantProtected = false }},
		{"单向绑定", func(r *CredentialRecord) { r.Tenant.Database = nil }},
		{"删除中的申请", func(r *CredentialRecord) { r.Tenant.Deleting = true }},
		{"申请尚未Bound", func(r *CredentialRecord) { r.Tenant.Phase = binding.Binding }},
		{"旧申请身份", func(r *CredentialRecord) { r.Tenant.Identity.UID = "new-tenant" }},
		{"旧资源身份", func(r *CredentialRecord) { r.Tenant.Database.UID = "new-database" }},
		{"Instance未出现", func(r *CredentialRecord) { r.Instance = nil }},
		{"Instance正在删除", func(r *CredentialRecord) { r.Instance.Deleting = true }},
		{"Instance未Ready", func(r *CredentialRecord) { r.Instance.Ready = false }},
		{"Instance身份未记录", func(r *CredentialRecord) { r.Database.InstanceUID = "" }},
		{"Instance同名重建", func(r *CredentialRecord) { r.Instance.Identity.UID = "new-instance" }},
		{"目标不一致", func(r *CredentialRecord) { r.Database.LoginRole = "another_owner" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := preparationRecord(t)
			test.change(record)
			resources := &memoryCredentialResources{record: record}
			store := &preparationStore{readError: ErrCredentialNotFound}
			if err := (CredentialPreparation{Resources: resources, Store: store}).Reconcile(t.Context(), record.Database.Identity.Name); err != nil {
				t.Fatal(err)
			}
			if store.reads != 0 || store.creates != 0 || resources.record.Status.Version != 0 {
				t.Fatal("前置条件不满足时不得读取、创建或确认凭据")
			}
		})
	}
}

func TestCredentialPreparationWriteBoundary(t *testing.T) {
	for _, test := range []struct {
		name        string
		saveError   bool
		stale       bool
		createError error
		wantError   bool
		wantCreates int
		wantVersion int64
	}{
		{name: "位置无法保存", saveError: true, wantError: true},
		{name: "外部操作前快照变化", stale: true, wantError: true},
		{name: "明确权限拒绝", createError: ErrCredentialUnavailable, wantCreates: 1},
		{name: "创建结果不确定", createError: ErrCredentialUncertain, wantCreates: 1},
		{name: "并发创建冲突", createError: ErrCredentialConflict, wantCreates: 1},
		{name: "创建与回读成功", wantCreates: 1, wantVersion: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			resources := &memoryCredentialResources{record: preparationRecord(t)}
			if test.saveError {
				resources.saveError = errors.New("fixture write failure")
			}
			if test.stale {
				resources.checkError = errors.New("fixture stale observation")
			}
			store := &preparationStore{readError: ErrCredentialNotFound, createError: test.createError}
			service := CredentialPreparation{Resources: resources, Store: store}
			err := service.Reconcile(t.Context(), resources.record.Database.Identity.Name)
			if (err != nil) != test.wantError || store.creates != test.wantCreates || resources.record.Status.Version != test.wantVersion {
				t.Fatal("外部写入边界或确认时机不符合预期")
			}
			if test.createError == ErrCredentialUncertain || test.stale {
				resources.checkError = nil
				if err := service.Reconcile(t.Context(), resources.record.Database.Identity.Name); err != nil {
					t.Fatal(err)
				}
				if store.creates != test.wantCreates || resources.record.Status.Phase != credentialdomain.Conflict {
					t.Fatal("未确认创建重入时不得生成替代密码")
				}
			}
		})
	}
}
