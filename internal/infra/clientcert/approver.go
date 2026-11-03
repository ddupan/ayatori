package clientcert

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"slices"
	"strings"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// +kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests/approval,verbs=update
// +kubebuilder:rbac:groups=certificates.k8s.io,resources=signers,resourceNames=kubernetes.io/kube-apiserver-client,verbs=approve

// RenewalApprover 批准身份以现有证书为自己续签的 CSR。首次签发不经过它，由部署方核对后批准：
// bootstrap token 本身不能换到任何证书。
type RenewalApprover struct {
	Client client.Client
}

func (r *RenewalApprover) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("client-certificate-renewal").
		For(&certificatesv1.CertificateSigningRequest{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			csr, ok := o.(*certificatesv1.CertificateSigningRequest)
			return ok && csr.Spec.SignerName == certificatesv1.KubeAPIServerClientSignerName && !decided(csr)
		}))).
		Complete(r)
}

func (r *RenewalApprover) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var csr certificatesv1.CertificateSigningRequest
	if err := r.Client.Get(ctx, req.NamespacedName, &csr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if decided(&csr) {
		return ctrl.Result{}, nil
	}
	// 不符合的 CSR 不拒绝：可能是部署方正要核对的首次申请。
	if err := IsRenewal(&csr); err != nil {
		ctrl.LoggerFrom(ctx).V(1).Info("not a renewal", "reason", err.Error())
		return ctrl.Result{}, nil
	}
	csr.Status.Conditions = append(csr.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
		Type:           certificatesv1.CertificateApproved,
		Status:         corev1.ConditionTrue,
		Reason:         "AyatoriClientCertificateRenewal",
		Message:        "the requester renews its own client certificate",
		LastUpdateTime: metav1.Now(),
	})
	return ctrl.Result{}, r.Client.SubResource("approval").Update(ctx, &csr)
}

// IsRenewal 判断 CSR 是否为身份为自己续签：申请者带 Group，证书的 CN 与 O 恰好是申请者自己的用户名与组。
// 满足时，签出的证书不会比申请者已有的权限更多。
func IsRenewal(csr *certificatesv1.CertificateSigningRequest) error {
	spec := csr.Spec
	if !slices.Contains(spec.Groups, Group) {
		return fmt.Errorf("requester %s is not in %s", spec.Username, Group)
	}
	if strings.HasPrefix(spec.Username, "system:serviceaccount:") || strings.HasPrefix(spec.Username, "system:bootstrap:") {
		return fmt.Errorf("requester %s", spec.Username)
	}
	groups := slices.DeleteFunc(slices.Clone(spec.Groups), func(g string) bool { return g == "system:authenticated" })
	return checkRequest(csr, spec.Username, groups)
}

// checkRequest 核对 CSR 只申请给定身份的客户端证书：CN 与 O（顺序无关）完全一致，只用于客户端认证，
// 没有任何 SAN。
func checkRequest(csr *certificatesv1.CertificateSigningRequest, user string, groups []string) error {
	spec := csr.Spec
	if spec.SignerName != certificatesv1.KubeAPIServerClientSignerName {
		return fmt.Errorf("signer %s", spec.SignerName)
	}
	block, _ := pem.Decode(spec.Request)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return fmt.Errorf("request is not a PEM certificate request")
	}
	req, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return err
	}
	if req.Subject.CommonName != user {
		return fmt.Errorf("common name %q, want %q", req.Subject.CommonName, user)
	}
	want, orgs := slices.Clone(groups), slices.Clone(req.Subject.Organization)
	slices.Sort(want)
	slices.Sort(orgs)
	if !slices.Equal(want, orgs) {
		return fmt.Errorf("organizations %v, want %v", orgs, want)
	}
	if len(req.DNSNames)+len(req.IPAddresses)+len(req.EmailAddresses)+len(req.URIs) > 0 {
		return fmt.Errorf("request has subject alternative names")
	}
	if !slices.Contains(spec.Usages, certificatesv1.UsageClientAuth) {
		return fmt.Errorf("usages %v lack client auth", spec.Usages)
	}
	for _, u := range spec.Usages {
		switch u {
		case certificatesv1.UsageClientAuth, certificatesv1.UsageDigitalSignature, certificatesv1.UsageKeyEncipherment:
		default:
			return fmt.Errorf("usage %s", u)
		}
	}
	return nil
}

func decided(csr *certificatesv1.CertificateSigningRequest) bool {
	for _, c := range csr.Status.Conditions {
		if c.Type == certificatesv1.CertificateApproved || c.Type == certificatesv1.CertificateDenied {
			return true
		}
	}
	return false
}
