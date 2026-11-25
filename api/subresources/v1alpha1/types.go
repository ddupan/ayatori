// Package v1alpha1 是 ayatori-api 提供的聚合子资源（ADR-0027）的组与响应类型。这些子资源不存储，
// 主资源仍是各自组里的 CRD；RBAC 按本组授权，例如 postgresqltenants/credentials 的 get。
// 不是 CRD，controller-gen 跳过本包。
// +kubebuilder:skip
package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	Group   = "subresources.ayatori.ddupan.top"
	Version = "v1alpha1"
)

// DatabaseCredentials 是 postgresqltenants/credentials 的响应：Tenant 已确认的应用凭据，取自 OpenBao。
type DatabaseCredentials struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	// Version 是凭据在 OpenBao 中的版本，即 Tenant status 的 credentialVersion。
	Version int64  `json:"version"`
	Host    string `json:"host"`
	// HostAddr 是可选的 IP（libpq hostaddr），给出时连接它、以 Host 校验证书。
	HostAddr string `json:"hostaddr,omitempty"`
	Port     string `json:"port"`
	SSLMode  string `json:"sslmode"`
	Database string `json:"database"`
	Username string `json:"username"`
	Password string `json:"password"`
}
