package application

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/binding"
)

const (
	bindingReadTenant        = "tenant"
	bindingBegin             = "begin"
	bindingReadInstance      = "instance"
	bindingCreate            = "create"
	bindingRecordInstance    = "record-instance"
	bindingWriteResource     = "bind"
	bindingDatabaseOperation = "database"
	bindingTestNamespace     = "apps"
	bindingTestName          = "app"
	bindingTestInstance      = "shared"
)

func TestBindingServiceOrder(t *testing.T) {
	resources := bindingFixture()
	service := BindingService{Resources: resources}
	result, err := service.Reconcile(t.Context(), bindingTestNamespace, bindingTestName)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{bindingReadTenant, bindingBegin, bindingDatabaseOperation, bindingReadInstance, bindingCreate, bindingRecordInstance, bindingWriteResource, bindingReadTenant, bindingDatabaseOperation}
	if !reflect.DeepEqual(resources.calls, want) {
		t.Fatalf("协调顺序 = %v, want %v", resources.calls, want)
	}
	if result.Status.Phase != binding.Bound || result.Status.Database == nil || result.Status.Database.UID != "database-uid" {
		t.Fatalf("绑定结果不符: %+v", result.Status)
	}
	if resources.tenant.Phase != binding.Binding || resources.tenant.Database != nil {
		t.Fatal("service 只能返回待呈现结果，不能提前写申请侧绑定")
	}
	if resources.database.Tenant == nil || *resources.database.Tenant != resources.tenant.Identity {
		t.Fatal("返回完成结果之前必须写入资源侧绑定")
	}
}

func TestBindingServiceStopsOnIOFailure(t *testing.T) {
	for _, operation := range []string{bindingReadTenant, bindingBegin, bindingDatabaseOperation, bindingReadInstance, bindingCreate, bindingRecordInstance, bindingWriteResource} {
		t.Run(operation, func(t *testing.T) {
			resources := bindingFixture()
			resources.failAt = operation
			result, err := (BindingService{Resources: resources}).Reconcile(t.Context(), bindingTestNamespace, bindingTestName)
			if !errors.Is(err, errBindingTest) || result.Status.Database != nil {
				t.Fatalf("IO 失败不应被转换为绑定成功: result=%+v, err=%v", result, err)
			}
			if resources.calls[len(resources.calls)-1] != operation {
				t.Fatalf("失败后继续执行了操作: %v", resources.calls)
			}
		})
	}
}

func TestBindingServiceDoesNotWriteWhenDeletingOrInvalid(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		resources := bindingFixture()
		resources.tenant.Deleting = deleting
		resources.tenant.Request = binding.Request{}
		result, err := (BindingService{Resources: resources}).Reconcile(t.Context(), bindingTestNamespace, bindingTestName)
		if err != nil || len(resources.calls) != 1 || result.Status.Database != nil {
			t.Fatalf("删除或无效申请不应触及资源: calls=%v, err=%v", resources.calls, err)
		}
		want := "InvalidRequest"
		if deleting {
			want = "DeletionPending"
		}
		if result.Status.Reason != want {
			t.Fatalf("Reason = %s, want %s", result.Status.Reason, want)
		}
	}
}

func TestBindingServiceConfirmsIdentityAgain(t *testing.T) {
	resources := bindingFixture()
	resources.replaceOnReadback = true
	result, err := (BindingService{Resources: resources}).Reconcile(t.Context(), bindingTestNamespace, bindingTestName)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status.Reason != binding.Conflict || result.Status.Database != nil {
		t.Fatal("资源侧写入后发生身份变化时不能返回绑定完成")
	}
}

// 这里只记录用例操作，不模拟 Kubernetes 校验；真实 IO 契约由 controller envtest 覆盖。
type bindingTestResources struct {
	tenant            *BindingTenant
	database          *BindingDatabase
	instance          *binding.Instance
	calls             []string
	failAt            string
	replaceOnReadback bool
	bound             bool
}

var errBindingTest = errors.New("injected resource operation failure")

func bindingFixture() *bindingTestResources {
	tenant := &BindingTenant{Generation: 1}
	tenant.Tenant = binding.Tenant{
		Identity: binding.TenantIdentity{Namespace: bindingTestNamespace, Name: bindingTestName, UID: "tenant-uid"},
		Request:  binding.Request{Provision: &binding.ProvisionRequest{Instance: bindingTestInstance}},
	}
	return &bindingTestResources{
		tenant:   tenant,
		instance: &binding.Instance{Identity: binding.Identity{Name: bindingTestInstance, UID: "instance-uid"}, Ready: true},
	}
}

func (r *bindingTestResources) record(operation string) error {
	r.calls = append(r.calls, operation)
	if r.failAt == operation {
		return errBindingTest
	}
	return nil
}

func (r *bindingTestResources) Tenant(context.Context, string, string) (*BindingTenant, error) {
	return r.tenant, r.record(bindingReadTenant)
}

func (r *bindingTestResources) Database(context.Context, string) (*BindingDatabase, error) {
	if r.bound && r.replaceOnReadback {
		replaced := *r.database
		replaced.Identity.UID = "replacement"
		return &replaced, r.record(bindingDatabaseOperation)
	}
	return r.database, r.record(bindingDatabaseOperation)
}

func (r *bindingTestResources) Instance(context.Context, string) (*binding.Instance, error) {
	return r.instance, r.record(bindingReadInstance)
}

func (r *bindingTestResources) BeginBinding(_ context.Context, tenant *BindingTenant, status *BindingStatus) (*BindingTenant, error) {
	if err := r.record(bindingBegin); err != nil {
		return nil, err
	}
	if status != nil {
		r.tenant.Phase = status.Phase
	}
	return tenant, nil
}

func (r *bindingTestResources) CreateDatabase(_ context.Context, target binding.Target, tenant binding.TenantIdentity) (*BindingDatabase, error) {
	if err := r.record(bindingCreate); err != nil {
		return nil, err
	}
	r.database = &BindingDatabase{}
	r.database.Database = binding.Database{
		Identity: binding.Identity{Name: target.Name, UID: "database-uid"}, Tenant: &tenant,
		Instance: target.Provision.Instance, Name: target.Provision.Database, LoginRole: target.Provision.LoginRole, Source: "Provision",
	}
	return r.database, nil
}

func (r *bindingTestResources) RecordInstance(_ context.Context, database *BindingDatabase, uid string) (*BindingDatabase, error) {
	if err := r.record(bindingRecordInstance); err != nil {
		return nil, err
	}
	database.InstanceUID = uid
	return database, nil
}

func (r *bindingTestResources) BindDatabase(_ context.Context, database *BindingDatabase, tenant binding.TenantIdentity) (*BindingDatabase, error) {
	if err := r.record(bindingWriteResource); err != nil {
		return nil, err
	}
	database.Tenant = &tenant
	r.bound = true
	return database, nil
}
