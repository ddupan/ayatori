package cli

import (
	"context"
	"encoding/json"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	subresourcesv1alpha1 "go.ddupan.top/ayatori/api/subresources/v1alpha1"
)

// RESTConfig 以 Token 选出的凭据连接 profile 的 apiserver。
func (s Store) RESTConfig(ctx context.Context, name string, p Profile) (*rest.Config, error) {
	token, err := s.Token(ctx, name, p)
	if err != nil {
		return nil, err
	}
	return &rest.Config{Host: p.Server, BearerToken: token, UserAgent: "ayatori",
		CAData: []byte(p.CertificateAuthority)}, nil
}

// WhoAmI 返回 apiserver 认定的用户与组（SelfSubjectReview）。
func WhoAmI(ctx context.Context, config *rest.Config) (authenticationv1.UserInfo, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return authenticationv1.UserInfo{}, err
	}
	review, err := clientset.AuthenticationV1().SelfSubjectReviews().Create(ctx,
		&authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return authenticationv1.UserInfo{}, err
	}
	return review.Status.UserInfo, nil
}

// DatabaseCredentials 读 Tenant 的 postgresqltenants/credentials 子资源（ADR-0027）：apiserver 按 RBAC
// 授权后由 ayatori-api 从 OpenBao 取出，CLI 不需要知道 OpenBao。
func DatabaseCredentials(ctx context.Context, config *rest.Config, namespace, name string) (subresourcesv1alpha1.DatabaseCredentials, error) {
	var c subresourcesv1alpha1.DatabaseCredentials
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return c, err
	}
	body, err := clientset.RESTClient().Get().
		AbsPath("/apis", subresourcesv1alpha1.Group, subresourcesv1alpha1.Version,
			"namespaces", namespace, "postgresqltenants", name, "credentials").
		Do(ctx).Raw()
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(body, &c)
}
