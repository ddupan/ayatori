package agent

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"git.ddupan.top/panxiao81/ayatori/internal/infra/clientcert"
	"git.ddupan.top/panxiao81/ayatori/internal/infra/node"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/loadbalancer"
)

// Role 是 LB 主机的角色（ADR-0022）：Node label node-role.kubernetes.io/loadbalancer，
// condition loadbalancer.ayatori.ddupan.top/Ready，Lease <node>-loadbalancer。
const Role = "loadbalancer"

// Node condition 的 Reason。
const (
	reasonAgentReady   = "AgentReady"
	reasonHAProxyDown  = "HAProxyDown"
	reasonNoBGPSession = "NoBGPSession"
)

// startCredentials 在 config.NodeName 设置时准备访问 API 的配置。用证书身份时启动申请与续签：
// 证书尚未签发时 restConfig 返回错误，宣告不受影响。返回的 stop 停止续签。
func startCredentials(config Config, registry prometheus.Registerer) (restConfig func() (*rest.Config, error), stop func(), err error) {
	stop = func() {}
	if config.NodeName == "" {
		return nil, stop, nil
	}
	if config.CredentialsDir == "" {
		return func() (*rest.Config, error) { return clientcmd.BuildConfigFromFlags("", config.Kubeconfig) }, stop, nil
	}
	dir := config.CredentialsDir
	if stop, err = clientcert.Start(context.Background(), node.Identity(dir, config.NodeName, Role)); err != nil {
		return nil, nil, err
	}
	// 控制面的证书到期指标覆盖不到主机上的证书；agent 停机超过剩余有效期就要重新首次签发（ADR-0022）。
	registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "ayatori_lb_agent_client_certificate_not_after_timestamp_seconds",
		Help: "agent 客户端证书的到期时间；尚未签发时为 0。",
	}, func() float64 {
		notAfter, err := clientcert.NotAfter(dir)
		if err != nil {
			return 0
		}
		return float64(notAfter.Unix())
	}))
	return func() (*rest.Config, error) { return clientcert.RESTConfig(dir) }, stop, nil
}

// register 等到能构造 API 配置（证书已签发）后注册 Node、续约 Lease，并注册与 Node 同名的
// LoadBalancerHost、写它的 status，直到 ctx 结束。控制面不可用时宣告照常。
func (a *Agent) register(ctx context.Context) {
	var config *rest.Config
	for {
		var err error
		if config, err = a.restConfig(); err == nil {
			break
		}
		a.logger.Debug("API credentials not ready", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		a.logger.Error("build scheme", "error", err)
		return
	}
	kube, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		a.logger.Error("build API client", "error", err)
		return
	}
	logf := func(msg string, err error) { a.logger.Debug(msg, "error", err) }
	go a.reportHost(ctx, kube, logf)
	r := &node.Registrar{Client: kube, Node: a.config.NodeName, Role: Role}
	r.Run(ctx, func() node.Status { return nodeStatus(a.Status()) }, logf)
}

// nodeStatus 把本机事实转为 loadbalancer 角色的就绪：HAProxy 应答且至少一个 BGP 会话已建立，
// 本机才能承接新的 VIP。单个 VIP 是否宣告不影响它，那是 LoadBalancer 的 status。
func nodeStatus(s loadbalancer.AgentStatus) node.Status {
	switch {
	case !s.HAProxyUp:
		return node.Status{Reason: reasonHAProxyDown, Message: "HAProxy stats socket 不应答"}
	case !slices.ContainsFunc(s.Neighbors, func(n loadbalancer.NeighborStatus) bool { return n.Established }):
		return node.Status{Reason: reasonNoBGPSession, Message: fmt.Sprintf("%d 个 BGP 邻居均未建立会话", len(s.Neighbors))}
	default:
		return node.Status{Ready: true, Reason: reasonAgentReady}
	}
}
