// Package v1alpha1 定义 Database 领域的 Kubernetes API。
// +kubebuilder:object:generate=true
// +groupName=database.ayatori.ddupan.top
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	SchemeGroupVersion = schema.GroupVersion{Group: "database.ayatori.ddupan.top", Version: "v1alpha1"}
	GroupVersion       = SchemeGroupVersion
	SchemeBuilder      = runtime.NewSchemeBuilder(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion,
			&PostgreSQLInstance{}, &PostgreSQLInstanceList{},
			&PostgreSQLDatabase{}, &PostgreSQLDatabaseList{},
			&PostgreSQLTenant{}, &PostgreSQLTenantList{},
		)
		metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
		return nil
	})
	AddToScheme = SchemeBuilder.AddToScheme
)
