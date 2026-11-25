// Package v1alpha1 是 libvirt/LXC 计算后端的 API（ADR-0015）：透传原生 XML，由各节点的 Compute Agent 下发。
// +kubebuilder:object:generate=true
// +groupName=compute.ayatori.ddupan.top
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	SchemeGroupVersion = schema.GroupVersion{Group: "compute.ayatori.ddupan.top", Version: "v1alpha1"}
	GroupVersion       = SchemeGroupVersion
	SchemeBuilder      = runtime.NewSchemeBuilder(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion,
			&LibvirtNetwork{}, &LibvirtNetworkList{},
			&LibvirtStoragePool{}, &LibvirtStoragePoolList{},
			&LibvirtStorageVolume{}, &LibvirtStorageVolumeList{},
			&LibvirtDomain{}, &LibvirtDomainList{},
			&LXCContainer{}, &LXCContainerList{},
			&Image{}, &ImageList{},
		)
		metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
		return nil
	})
	AddToScheme = SchemeBuilder.AddToScheme
)
