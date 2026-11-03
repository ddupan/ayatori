// Package clientcert 管理访问 Ayatori apiserver 的客户端证书，做法与 kubelet 的 TLS bootstrap 相同
// （ADR-0021）：私钥在使用它的机器上生成；首次以 bootstrap token 提交 CSR，由部署方核对后批准；之后
// 持有者在有效期过去 70–90% 时以现有证书提交 CSR 自行续签，由 ayatori-manager 的批准者批准。
//
// 一个身份是一个目录：
//   - bootstrap-kubeconfig：部署方写入，提供 apiserver 地址、CA 与 bootstrap token；token 过期后只用前两者；
//   - kubeconfig：本包写出，证书与私钥引用同目录的 client.pem。client-go 会定期重新加载按路径引用的
//     证书文件，所以使用它的进程（包括 Flux 等第三方组件）不需要重启就能用上续签后的证书。
package clientcert

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	certificatesv1 "k8s.io/api/certificates/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/util/certificate"
)

const (
	// Group 是本模式签发的所有证书都带的组。批准者只为带此组的身份自动批准续签，
	// 以免 ServiceAccount、OIDC 用户借续签换得长期证书。
	Group = "system:ayatori:client-certificates"
	// BootstrapKubeconfig 与 Kubeconfig 是身份目录中的文件名。
	BootstrapKubeconfig = "bootstrap-kubeconfig"
	Kubeconfig          = "kubeconfig"
	certFile            = "client.pem"
)

// Identity 描述一个身份。证书的 CN 为 CommonName，O 为 Organization 加上 Group。
type Identity struct {
	Dir          string
	CommonName   string
	Organization []string
	// UID 不为 -1 时，写出的文件属于该用户：续签程序以 root 替控制面容器写证书。
	UID int
}

// Start 写出 kubeconfig 并启动证书管理器；没有有效证书时立即申请。返回的 stop 停止续签。
func Start(ctx context.Context, id Identity) (stop func(), err error) {
	boot, err := bootstrapConfig(id.Dir)
	if err != nil {
		return nil, err
	}
	store := &fileStore{path: filepath.Join(id.Dir, certFile), uid: id.UID}
	if err := writeKubeconfig(id, boot); err != nil {
		return nil, err
	}
	manager, err := certificate.NewManager(&certificate.Config{
		Ctx:  &ctx,
		Name: id.CommonName,
		// current 由管理器传入，只在证书有效时不为 nil；没有有效证书时以 bootstrap token 申请。
		// 每次重新读取 bootstrap-kubeconfig：部署方为过期的身份重新写入 token 后，不必重启。
		ClientsetFn: func(current *tls.Certificate) (kubernetes.Interface, error) {
			config, err := bootstrapConfig(id.Dir)
			if err != nil {
				return nil, err
			}
			if current != nil {
				config = &rest.Config{Host: config.Host, CAData: config.CAData, CertFile: store.path, KeyFile: store.path}
			}
			return kubernetes.NewForConfig(config)
		},
		Template: &x509.CertificateRequest{Subject: pkix.Name{
			CommonName: id.CommonName, Organization: append(append([]string(nil), id.Organization...), Group)}},
		SignerName:       certificatesv1.KubeAPIServerClientSignerName,
		Usages:           []certificatesv1.KeyUsage{certificatesv1.UsageDigitalSignature, certificatesv1.UsageClientAuth},
		CertificateStore: store,
	})
	if err != nil {
		return nil, err
	}
	manager.Start()
	return manager.Stop, nil
}

// RESTConfig 读取身份目录中的 kubeconfig。
func RESTConfig(dir string) (*rest.Config, error) {
	return clientcmd.BuildConfigFromFlags("", filepath.Join(dir, Kubeconfig))
}

func bootstrapConfig(dir string) (*rest.Config, error) {
	config, err := clientcmd.BuildConfigFromFlags("", filepath.Join(dir, BootstrapKubeconfig))
	if err != nil {
		return nil, fmt.Errorf("load bootstrap kubeconfig: %w", err)
	}
	if len(config.CAData) == 0 && config.CAFile != "" {
		if config.CAData, err = os.ReadFile(config.CAFile); err != nil {
			return nil, err
		}
	}
	return config, nil
}

func writeKubeconfig(id Identity, boot *rest.Config) error {
	data, err := clientcmd.Write(clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{"ayatori": {Server: boot.Host, CertificateAuthorityData: boot.CAData}},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{id.CommonName: {ClientCertificate: certFile, ClientKey: certFile}},
		Contexts:       map[string]*clientcmdapi.Context{"default": {Cluster: "ayatori", AuthInfo: id.CommonName}},
		CurrentContext: "default",
	})
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(id.Dir, Kubeconfig), data, id.UID)
}

// fileStore 把证书与私钥写在同一个文件里，整体原子替换：分成两个文件时，client-go 可能在两次
// rename 之间读到不配对的证书与私钥。
type fileStore struct {
	path string
	uid  int
}

func (s *fileStore) Current() (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(s.path, s.path)
	if errors.Is(err, os.ErrNotExist) {
		missing := certificate.NoCertKeyError(s.path + " does not exist")
		return nil, &missing
	}
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

func (s *fileStore) Update(certPEM, keyPEM []byte) (*tls.Certificate, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	if err := writeFile(s.path, append(append([]byte(nil), certPEM...), keyPEM...), s.uid); err != nil {
		return nil, err
	}
	return &cert, nil
}

// writeFile 先写临时文件再 rename，读者要么看到旧内容，要么看到新内容。
func writeFile(path string, data []byte, uid int) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o400); err != nil {
		return err
	}
	if uid != -1 {
		if err := os.Chown(tmp.Name(), uid, uid); err != nil {
			return err
		}
	}
	return os.Rename(tmp.Name(), path)
}
