package controller

import (
	"context"
	"errors"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

// InstanceCacheOptions 必须在创建 manager 时使用；只 watch 固定 namespace 的 Secret metadata。
// SecretCredentials 始终直读 API，不会令共享 cache 保存密码。
func InstanceCacheOptions(namespace string) cache.Options {
	return cache.Options{ByObject: map[client.Object]cache.ByObject{
		&corev1.Secret{}: {Namespaces: map[string]cache.Config{namespace: {}}},
	}}
}

func (r *InstanceReconciler) SetupWithManager(manager ctrl.Manager) error {
	if r.Observer == nil || len(validation.IsDNS1123Label(r.SecretNamespace)) != 0 {
		return errors.New("instance observer and valid management Secret namespace required")
	}
	if r.Client == nil {
		r.Client = manager.GetClient()
	}
	if r.Reader == nil {
		r.Reader = manager.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(manager).
		Named("database-instance").
		For(&databasev1alpha1.PostgreSQLInstance{}).
		WatchesMetadata(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.instancesForSecret)).
		Watches(&databasev1alpha1.PostgreSQLDatabase{}, handler.EnqueueRequestsFromMapFunc(r.instanceForReference)).
		Watches(&databasev1alpha1.PostgreSQLTenant{}, handler.EnqueueRequestsFromMapFunc(r.instanceForReference)).
		Complete(r)
}

func (r *InstanceReconciler) instancesForSecret(ctx context.Context, object client.Object) []ctrl.Request {
	if object.GetNamespace() != r.SecretNamespace {
		return nil
	}
	instances := &databasev1alpha1.PostgreSQLInstanceList{}
	if err := r.Client.List(ctx, instances); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "无法映射管理 Secret 事件；等待低频重试")
		return nil
	}
	var requests []ctrl.Request
	for _, item := range instances.Items {
		if string(item.Spec.AdminCredentialRef.Name) == object.GetName() {
			request := ctrl.Request{Name: item.Name}
			requests = append(requests, request)
		}
	}
	return requests
}

func (r *InstanceReconciler) instanceForReference(_ context.Context, object client.Object) []ctrl.Request {
	var name string
	switch item := object.(type) {
	case *databasev1alpha1.PostgreSQLDatabase:
		name = string(item.Spec.InstanceRef.Name)
	case *databasev1alpha1.PostgreSQLTenant:
		if item.Spec.Provision != nil {
			name = string(item.Spec.Provision.InstanceRef.Name)
		}
	}
	if name == "" {
		return nil
	}
	request := ctrl.Request{Name: name}
	return []ctrl.Request{request}
}
