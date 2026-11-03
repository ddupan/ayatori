//go:build integration

package clientcert_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	authenticationv1 "k8s.io/api/authentication/v1"
	certificatesv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/transport"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"git.ddupan.top/panxiao81/ayatori/internal/infra/clientcert"
	"git.ddupan.top/panxiao81/ayatori/internal/testenv"
)

func eventually(t *testing.T, timeout time.Duration, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s: %v", timeout, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestClientCertificates(t *testing.T) {
	// client-go 默认每 5 分钟重新加载证书文件；测试中的证书只有几十秒。
	transport.CertCallbackRefreshDuration = time.Second
	ctrl.SetLogger(logr.Discard())
	ctx := t.Context()
	authority := testenv.NewClientCA(t)
	env := &envtest.Environment{}
	api := env.ControlPlane.GetAPIServer()
	api.Authn = authority
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	admin := kubernetes.NewForConfigOrDie(cfg)
	testenv.ApplyManifest(t, cfg, filepath.Join("testdata", "rbac.yaml"))

	// 证书 40 秒到期：证书管理器在 70–90% 时续签，测试在一分钟内看到续签和过期后的重新加载。
	go authority.SignApproved(ctx, admin, 40*time.Second)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&clientcert.RenewalApprover{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(ctx) }()

	dir := t.TempDir()
	const tokenID, user = "abcdef", "test-component"
	testenv.BootstrapToken(t, admin, cfg, dir, tokenID)
	stop, err := clientcert.Start(ctx, clientcert.Identity{Dir: dir, CommonName: user, UID: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	t.Run("bootstrap token alone gets no certificate", func(t *testing.T) {
		var csr *certificatesv1.CertificateSigningRequest
		eventually(t, 30*time.Second, func() error {
			csr = requestedBy(t, admin, "system:bootstrap:"+tokenID)
			if csr == nil {
				return fmt.Errorf("no CSR from the bootstrap token yet")
			}
			return nil
		})
		time.Sleep(2 * time.Second)
		if csr = requestedBy(t, admin, "system:bootstrap:"+tokenID); testenv.Approved(csr) {
			t.Fatal("the renewal approver approved a bootstrap request")
		}
	})

	t.Run("deployer rejects a request for another identity", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := clientcert.ApproveBootstrap(ctx, admin, tokenID, "someone-else", nil, 0); err == nil {
			t.Fatal("approved a CSR for another identity")
		}
	})

	var first *x509.Certificate
	t.Run("deployer approves the first certificate", func(t *testing.T) {
		// 被拒绝后证书管理器会以同一 token 重新申请。
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if err := clientcert.ApproveBootstrap(ctx, admin, tokenID, user, nil, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		eventually(t, 30*time.Second, func() error {
			var err error
			first, err = current(dir)
			return err
		})
		whoami(t, dir, user)
	})

	t.Run("renews itself and clients reload the file", func(t *testing.T) {
		config, err := clientcert.RESTConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		long := kubernetes.NewForConfigOrDie(config)
		eventually(t, time.Minute, func() error {
			cert, err := current(dir)
			if err != nil {
				return err
			}
			if cert.SerialNumber.Cmp(first.SerialNumber) == 0 {
				return fmt.Errorf("not renewed yet")
			}
			return nil
		})
		// 等第一张证书过期：同一个客户端仍能访问，说明它已换用续签后的证书。
		time.Sleep(time.Until(first.NotAfter) + 2*time.Second)
		if _, err := long.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("client built before renewal stopped working: %v", err)
		}
	})
}

func requestedBy(t *testing.T, admin kubernetes.Interface, user string) *certificatesv1.CertificateSigningRequest {
	t.Helper()
	list, err := admin.CertificatesV1().CertificateSigningRequests().List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		if list.Items[i].Spec.Username == user {
			return &list.Items[i]
		}
	}
	return nil
}

func current(dir string) (*x509.Certificate, error) {
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "client.pem"), filepath.Join(dir, "client.pem"))
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(pair.Certificate[0])
}

func whoami(t *testing.T, dir, user string) {
	t.Helper()
	config, err := clientcert.RESTConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	review, err := kubernetes.NewForConfigOrDie(config).AuthenticationV1().SelfSubjectReviews().
		Create(t.Context(), &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	info := review.Status.UserInfo
	if info.Username != user || !slices.Contains(info.Groups, clientcert.Group) {
		t.Fatalf("authenticated as %s %v", info.Username, info.Groups)
	}
}
