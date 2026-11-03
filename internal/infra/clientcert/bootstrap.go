package clientcert

import (
	"context"
	"fmt"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

// BootstrapGroup 是 bootstrap token 的 auth-extra-groups：它只能提交 CSR，不能换到证书。
const BootstrapGroup = "system:bootstrappers:ayatori"

// ApproveBootstrap 是部署方的首次签发：等待以 bootstrap token tokenID 提交的 CSR，核对它申请的正是
// user 与 groups（加上 Group）后批准。不符合的拒绝：这个 token 只为这一个身份创建，申请别的身份说明有问题。
func ApproveBootstrap(ctx context.Context, admin kubernetes.Interface, tokenID, user string, groups []string) error {
	requester := "system:bootstrap:" + tokenID
	return wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		list, err := admin.CertificatesV1().CertificateSigningRequests().List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, nil
		}
		for i := range list.Items {
			csr := &list.Items[i]
			if csr.Spec.Username != requester || decided(csr) {
				continue
			}
			condition := certificatesv1.CertificateSigningRequestCondition{
				Type: certificatesv1.CertificateApproved, Status: corev1.ConditionTrue,
				Reason: "AyatoriBootstrap", Message: "approved by the deployer for " + user,
			}
			err := checkRequest(csr, user, append(append([]string(nil), groups...), Group))
			if err != nil {
				condition.Type, condition.Reason, condition.Message = certificatesv1.CertificateDenied, "AyatoriBootstrapMismatch", err.Error()
			}
			csr.Status.Conditions = append(csr.Status.Conditions, condition)
			if _, uerr := admin.CertificatesV1().CertificateSigningRequests().UpdateApproval(ctx, csr.Name, csr, metav1.UpdateOptions{}); uerr != nil {
				return false, nil
			}
			if err != nil {
				return false, fmt.Errorf("denied %s: %w", csr.Name, err)
			}
			return true, nil
		}
		return false, nil
	})
}
