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

// ApproveBootstrap 是部署方的首次签发：批准以 bootstrap token tokenID 提交、且申请的正是 user 与 groups
// （加上 Group）的 CSR；不符合的拒绝：这个 token 只为这一个身份创建，申请别的身份说明有问题。
//
// 同一个 token 可能提交了不止一个 CSR（持有者重启或重试时，前一个 CSR 不会再被等待），所以批准全部
// 符合的 CSR，并在批准第一个之后继续 settle 时间，批准之后才出现的。多签出的证书没有人使用。
func ApproveBootstrap(ctx context.Context, admin kubernetes.Interface, tokenID, user string, groups []string, settle time.Duration) error {
	requester := "system:bootstrap:" + tokenID
	want := append(append([]string(nil), groups...), Group)
	var approvedAt time.Time
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
			mismatch := checkRequest(csr, user, want)
			if mismatch != nil {
				condition.Type, condition.Reason, condition.Message = certificatesv1.CertificateDenied, "AyatoriBootstrapMismatch", mismatch.Error()
			}
			csr.Status.Conditions = append(csr.Status.Conditions, condition)
			if _, err := admin.CertificatesV1().CertificateSigningRequests().UpdateApproval(ctx, csr.Name, csr, metav1.UpdateOptions{}); err != nil {
				return false, nil
			}
			if mismatch != nil {
				return false, fmt.Errorf("denied %s: %w", csr.Name, mismatch)
			}
			if approvedAt.IsZero() {
				approvedAt = time.Now()
			}
		}
		return !approvedAt.IsZero() && time.Since(approvedAt) >= settle, nil
	})
}
