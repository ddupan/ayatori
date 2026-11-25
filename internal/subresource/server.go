// Package subresource 是 ayatori-api：apiserver 上的聚合子资源（ADR-0027）。它不存储任何对象，主资源
// 仍是 CRD；认证、授权与审计都在 kube-apiserver——请求到这里之前，apiserver 已按 RBAC 授权了该子资源
// （见 api/subresources/v1alpha1）。
package subresource

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "go.ddupan.top/ayatori/api/database/v1alpha1"
	subresourcesv1alpha1 "go.ddupan.top/ayatori/api/subresources/v1alpha1"
	databasebao "go.ddupan.top/ayatori/internal/database/adapter/openbao"
	credentialdomain "go.ddupan.top/ayatori/internal/database/domain/credential"
)

const prefix = "/apis/" + subresourcesv1alpha1.Group + "/" + subresourcesv1alpha1.Version

// Server 以自己的身份读 API 与 OpenBao：Kube 读主资源，Credentials 只能读 Ayatori 交付的凭据路径。
type Server struct {
	Kube        client.Reader
	Credentials *databasebao.Credentials
	FrontProxy  FrontProxy
	Logger      *slog.Logger
}

func (s Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+prefix, s.discovery)
	mux.HandleFunc("GET "+prefix+"/namespaces/{namespace}/postgresqltenants/{name}/credentials", s.credentials)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, err := s.FrontProxy.User(r)
		if err != nil {
			writeStatus(w, apierrors.NewUnauthorized(err.Error()))
			return
		}
		s.Logger.Info("request", "user", user, "method", r.Method, "path", r.URL.Path)
		mux.ServeHTTP(w, r)
	})
}

// discovery 是聚合层的可用性检查与客户端发现所读的资源列表。
func (s Server) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, metav1.APIResourceList{
		Kind:         "APIResourceList",
		APIVersion:   "v1",
		GroupVersion: subresourcesv1alpha1.Group + "/" + subresourcesv1alpha1.Version,
		APIResources: []metav1.APIResource{
			{Name: "postgresqltenants/credentials", Namespaced: true, Kind: "DatabaseCredentials", Verbs: []string{"get"}},
		},
	})
}

func (s Server) credentials(w http.ResponseWriter, r *http.Request) {
	key := types.NamespacedName{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
	var tenant databasev1alpha1.PostgreSQLTenant
	if err := s.Kube.Get(r.Context(), key, &tenant); err != nil {
		writeStatus(w, err)
		return
	}
	ref, version := tenant.Status.CredentialRef, tenant.Status.CredentialVersion
	if ref == nil || version == 0 {
		writeStatus(w, apierrors.NewConflict(databasev1alpha1.GroupVersion.WithResource("postgresqltenants").GroupResource(),
			key.Name, errors.New("credentials are not ready yet")))
		return
	}
	credential, err := s.Credentials.ReadCredential(r.Context(), credentialdomain.Location{Mount: ref.Mount, Path: ref.Path}, version)
	if err != nil {
		s.Logger.Error("read credentials", "tenant", key, "error", err)
		writeStatus(w, credentialError(err))
		return
	}
	data := credential.SecretData()
	field := func(k string) string { v, _ := data[k].(string); return v }
	writeJSON(w, http.StatusOK, subresourcesv1alpha1.DatabaseCredentials{
		Kind:       "DatabaseCredentials",
		APIVersion: subresourcesv1alpha1.Group + "/" + subresourcesv1alpha1.Version,
		Name:       key.Name,
		Namespace:  key.Namespace,
		Version:    version,
		Host:       field("host"), HostAddr: field("hostaddr"), Port: field("port"), SSLMode: field("sslmode"),
		Database: field("database"), Username: field("username"), Password: field("password"),
	})
}

// credentialError 不把 OpenBao 的细节返回给用户：位置越界或版本不符是服务端的问题，日志里有原因。
func credentialError(err error) error {
	switch {
	case errors.Is(err, databasebao.ErrUnavailable):
		return apierrors.NewServiceUnavailable("credential store is unavailable")
	case errors.Is(err, databasebao.ErrConflict), errors.Is(err, databasebao.ErrNotFound):
		return apierrors.NewConflict(databasev1alpha1.GroupVersion.WithResource("postgresqltenants").GroupResource(),
			"", errors.New("the published credentials do not match the credential store"))
	default:
		return apierrors.NewInternalError(errors.New("cannot read credentials"))
	}
}

func writeStatus(w http.ResponseWriter, err error) {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		status = apierrors.NewInternalError(err)
	}
	s := status.Status()
	s.TypeMeta = metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}
	writeJSON(w, int(s.Code), s)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
