package cli

import (
	"context"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// RESTConfig 以用户自己的 ID token 连接 profile 的 apiserver。
func (s Store) RESTConfig(ctx context.Context, name string, p Profile) (*rest.Config, error) {
	token, err := s.IDToken(ctx, name, p)
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
