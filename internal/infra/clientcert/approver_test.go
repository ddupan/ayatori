package clientcert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net/url"
	"testing"

	certificatesv1 "k8s.io/api/certificates/v1"
)

func request(t *testing.T, template x509.CertificateRequest) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &template, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func TestIsRenewal(t *testing.T) {
	const user = "system:ayatori:node:node-a"
	groups := []string{"system:ayatori:nodes", Group, "system:authenticated"}
	subject := pkix.Name{CommonName: user, Organization: []string{Group, "system:ayatori:nodes"}}
	usages := []certificatesv1.KeyUsage{certificatesv1.UsageDigitalSignature, certificatesv1.UsageClientAuth}
	csr := func(mutate func(*certificatesv1.CertificateSigningRequestSpec, *x509.CertificateRequest)) *certificatesv1.CertificateSigningRequest {
		spec := certificatesv1.CertificateSigningRequestSpec{
			SignerName: certificatesv1.KubeAPIServerClientSignerName, Username: user, Groups: groups, Usages: usages}
		template := x509.CertificateRequest{Subject: subject}
		if mutate != nil {
			mutate(&spec, &template)
		}
		spec.Request = request(t, template)
		return &certificatesv1.CertificateSigningRequest{Spec: spec}
	}
	if err := IsRenewal(csr(nil)); err != nil {
		t.Fatalf("own renewal rejected: %v", err)
	}
	for name, mutate := range map[string]func(*certificatesv1.CertificateSigningRequestSpec, *x509.CertificateRequest){
		"another identity": func(_ *certificatesv1.CertificateSigningRequestSpec, r *x509.CertificateRequest) {
			r.Subject.CommonName = "system:ayatori:node:node-b"
		},
		"extra group": func(_ *certificatesv1.CertificateSigningRequestSpec, r *x509.CertificateRequest) {
			r.Subject.Organization = append(r.Subject.Organization, "system:masters")
		},
		"dropped group": func(_ *certificatesv1.CertificateSigningRequestSpec, r *x509.CertificateRequest) {
			r.Subject.Organization = []string{Group}
		},
		"requester without the marker group": func(s *certificatesv1.CertificateSigningRequestSpec, r *x509.CertificateRequest) {
			s.Groups = []string{"system:ayatori:nodes", "system:authenticated"}
			r.Subject.Organization = []string{"system:ayatori:nodes"}
		},
		"bootstrap token": func(s *certificatesv1.CertificateSigningRequestSpec, r *x509.CertificateRequest) {
			s.Username = "system:bootstrap:abcdef"
			r.Subject.CommonName = s.Username
		},
		"server usage": func(s *certificatesv1.CertificateSigningRequestSpec, _ *x509.CertificateRequest) {
			s.Usages = []certificatesv1.KeyUsage{certificatesv1.UsageServerAuth, certificatesv1.UsageClientAuth}
		},
		"subject alternative name": func(_ *certificatesv1.CertificateSigningRequestSpec, r *x509.CertificateRequest) {
			r.URIs = []*url.URL{{Scheme: "spiffe", Host: "ddupan.top"}}
		},
		"other signer": func(s *certificatesv1.CertificateSigningRequestSpec, _ *x509.CertificateRequest) {
			s.SignerName = certificatesv1.KubeAPIServerClientKubeletSignerName
		},
	} {
		if err := IsRenewal(csr(mutate)); err == nil {
			t.Errorf("%s: approved", name)
		}
	}
}
