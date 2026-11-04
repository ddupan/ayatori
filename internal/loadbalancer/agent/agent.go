// Package agent 是 LB 主机上的 LB agent（ADR-0017）：按本机 HAProxy 的 frontend 状态，经进程内
// GoBGP 宣告或撤回 VIP /32。无状态：每轮从 HAProxy 现状计算应宣告集合，不依赖 controller。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	networkv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/network/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/address"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/loadbalancer"
)

// Config 是 LB 主机部署写入的本地配置；环境相关的值都在这里，不进入发布物。
type Config struct {
	ASN       uint32     `json:"asn"`
	RouterID  string     `json:"routerID"`
	Neighbors []Neighbor `json:"neighbors"`
	// VIPPool 是本环境的 VIP 段；只有 bind 地址在段内的 frontend 参与宣告。
	VIPPool       string `json:"vipPool"`
	HAProxySocket string `json:"haproxySocket"`
	// Interval 是收敛间隔，也是 frontend 消失到撤回的最长延迟。
	Interval       metav1.Duration `json:"interval"`
	MetricsAddress string          `json:"metricsAddress"`
	// NodeName 设置后 agent 加入控制面，注册为 Node（ADR-0022）；为空时只在本机宣告，迁移期间
	// 尚未首次签发的主机照常工作。
	NodeName string `json:"nodeName,omitempty"`
	// CredentialsDir 是客户端证书身份目录（ADR-0021）：部署方写入 bootstrap-kubeconfig，agent 以
	// system:ayatori:node:<nodeName> 申请证书并自行续签。
	CredentialsDir string `json:"credentialsDir,omitempty"`
	// Kubeconfig 与 CredentialsDir 二选一，用于开发与测试。
	Kubeconfig string `json:"kubeconfig,omitempty"`
	// DataPlane 设置后 agent watch LoadBalancer 与 Backend，经本机 Data Plane API 收敛 HAProxy
	// （ADR-0022）；需要 nodeName。迁移期间与 manager 的远程收敛二选一，按环境整体切换。
	DataPlane *DataPlane `json:"dataPlane,omitempty"`
}

// DataPlane 是本机 Data Plane API；只监听 loopback，凭据由 cookbook 在本机生成。
type DataPlane struct {
	URL          string `json:"url"`
	Username     string `json:"username"`
	PasswordFile string `json:"passwordFile"`
	// ConnectTimeout 是代理连接后端的超时，缺省 5 秒；与 manager 的 --loadbalancer-connect-timeout 相同。
	ConnectTimeout metav1.Duration `json:"connectTimeout,omitempty"`
	// Resolvers 是 DNS endpoint 使用的 resolvers section 名称；为空时只在启动时解析。
	Resolvers string `json:"resolvers,omitempty"`
}

type Neighbor struct {
	Address string `json:"address"`
	ASN     uint32 `json:"asn"`
	// Port 为零时使用 179；只供测试改写。
	Port uint32 `json:"port,omitempty"`
}

// LoadConfig 读取并校验配置；缺省间隔为 2 秒。
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // 路径来自启动参数，读取它正是本函数的职责
	if err != nil {
		return Config{}, err
	}
	var config Config
	if err := yaml.UnmarshalStrict(data, &config); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if config.Interval.Duration == 0 {
		config.Interval.Duration = 2 * time.Second
	}
	if d := config.DataPlane; d != nil && d.ConnectTimeout.Duration == 0 {
		d.ConnectTimeout.Duration = 5 * time.Second
	}
	return config, config.validate()
}

func (c Config) validate() error {
	var errs []error
	if c.ASN == 0 {
		errs = append(errs, errors.New("asn is required"))
	}
	if addr, err := netip.ParseAddr(c.RouterID); err != nil || !addr.Is4() {
		errs = append(errs, errors.New("routerID must be an IPv4 address"))
	}
	if len(c.Neighbors) == 0 {
		errs = append(errs, errors.New("at least one neighbor is required"))
	}
	for _, n := range c.Neighbors {
		if _, err := netip.ParseAddr(n.Address); err != nil || n.ASN == 0 {
			errs = append(errs, fmt.Errorf("neighbor %q needs an IP address and asn", n.Address))
		}
	}
	if _, err := c.pool(); err != nil {
		errs = append(errs, err)
	}
	if c.HAProxySocket == "" {
		errs = append(errs, errors.New("haproxySocket is required"))
	}
	if c.Interval.Duration < 0 {
		errs = append(errs, errors.New("interval must be positive"))
	}
	if c.NodeName != "" && (c.CredentialsDir == "") == (c.Kubeconfig == "") {
		errs = append(errs, errors.New("nodeName needs exactly one of credentialsDir and kubeconfig"))
	}
	if c.NodeName == "" && (c.CredentialsDir != "" || c.Kubeconfig != "" || c.DataPlane != nil) {
		errs = append(errs, errors.New("credentialsDir, kubeconfig and dataPlane need nodeName"))
	}
	if d := c.DataPlane; d != nil && (d.URL == "" || d.Username == "" || d.PasswordFile == "") {
		errs = append(errs, errors.New("dataPlane needs url, username and passwordFile"))
	}
	return errors.Join(errs...)
}

func (c Config) pool() (address.Pool, error) {
	prefix, err := netip.ParsePrefix(c.VIPPool)
	if err != nil || !prefix.Addr().Is4() {
		return address.Pool{}, fmt.Errorf("vipPool %q must be an IPv4 CIDR", c.VIPPool)
	}
	return address.Pool{Prefix: prefix.Masked()}, nil
}

type metrics struct {
	haproxyUp   prometheus.Gauge
	announced   *prometheus.GaugeVec
	established *prometheus.GaugeVec
	lastSuccess prometheus.Gauge
}

func newMetrics(registry prometheus.Registerer) *metrics {
	m := &metrics{
		haproxyUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ayatori_lb_agent_haproxy_up", Help: "本轮 HAProxy stats socket 是否应答。"}),
		announced: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ayatori_lb_agent_vip_announced", Help: "本机是否宣告该 VIP。"}, []string{"vip"}),
		established: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ayatori_lb_agent_bgp_session_established", Help: "到该邻居的 BGP 会话是否已建立。"}, []string{"neighbor"}),
		lastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ayatori_lb_agent_last_sync_success_timestamp_seconds", Help: "最近一次成功收敛宣告的时间。"}),
	}
	registry.MustRegister(m.haproxyUp, m.announced, m.established, m.lastSuccess)
	return m
}

// Agent 按间隔收敛；Run 返回前停止 speaker，会话随之断开。
type Agent struct {
	config  Config
	pool    address.Pool
	speaker *Speaker
	vips    VIPs
	metrics *metrics
	logger  *slog.Logger

	// restConfig 为 nil 时不加入控制面；用证书身份时，证书签发之前它返回错误。
	restConfig      func() (*rest.Config, error)
	stopCredentials func()

	mu     sync.Mutex
	status loadbalancer.AgentStatus
	// loadBalancers 是最近一轮本机收敛的结果；不在本机收敛时为空。
	loadBalancers []networkv1alpha1.HostLoadBalancerStatus
}

// New 启动 speaker；vips 在生产中为 Loopback。
func New(ctx context.Context, config Config, vips VIPs, registry prometheus.Registerer, logger *slog.Logger) (*Agent, error) {
	pool, err := config.pool()
	if err != nil {
		return nil, err
	}
	restConfig, stopCredentials, err := startCredentials(config, registry)
	if err != nil {
		return nil, err
	}
	speaker, err := StartSpeaker(ctx, config, logger)
	if err != nil {
		stopCredentials()
		return nil, err
	}
	return &Agent{config: config, pool: pool, speaker: speaker, vips: vips, metrics: newMetrics(registry), logger: logger,
		restConfig: restConfig, stopCredentials: stopCredentials}, nil
}

// Run 先按本机 HAProxy 现状收敛一轮宣告，再加入控制面（ADR-0022）：控制面不可用、证书尚未签发时
// 宣告照常。
func (a *Agent) Run(ctx context.Context) {
	defer a.speaker.Stop()
	defer a.stopCredentials()
	ticker := time.NewTicker(a.config.Interval.Duration)
	defer ticker.Stop()
	a.reconcile(ctx)
	if a.restConfig != nil {
		go a.register(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		a.reconcile(ctx)
	}
}

func (a *Agent) reconcile(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, a.config.Interval.Duration)
	defer cancel()
	frontends, err := Frontends(ctx, a.config.HAProxySocket)
	if err != nil {
		// 不应答即撤回全部：frontends 为 nil 时宣告规则返回空集合。
		a.logger.Warn("HAProxy 不应答，撤回全部 VIP", "error", err)
	}
	a.metrics.haproxyUp.Set(boolGauge(err == nil))
	reasons := loadbalancer.Check(a.pool, frontends, a.vips.Local)
	want := loadbalancer.Announcements(a.pool, frontends, a.vips.Local)
	synced := a.speaker.Sync(want)
	if synced != nil {
		a.logger.Error("收敛宣告失败", "error", synced)
	}
	// 地址在撤回之后才删除，在宣告之前就已存在：新 VIP 本轮成为本机地址，下一轮才宣告。
	// HAProxy 不应答时不知道哪些 VIP 仍有 bind，地址保持不变。
	if err == nil {
		if err := a.vips.Sync(loadbalancer.Bound(a.pool, frontends)); err != nil {
			a.logger.Error("收敛 VIP 地址失败", "error", err)
			synced = err
		}
	}
	a.mu.Lock()
	a.status.HAProxyUp = err == nil
	if synced == nil {
		now := time.Now()
		a.status.LastSync = &now
		a.metrics.lastSuccess.Set(float64(now.Unix()))
	}
	a.mu.Unlock()
	a.observe(ctx, reasons)
}

// observe 以本地 RIB 与 adj-rib-out 为准导出指标和 status，而不是以本轮期望为准。
func (a *Agent) observe(ctx context.Context, reasons map[netip.Addr]loadbalancer.Reason) {
	announced, err := a.speaker.Announced()
	if err == nil {
		a.metrics.announced.Reset()
		for vip := range announced {
			a.metrics.announced.WithLabelValues(vip.String()).Set(1)
		}
	}
	peers, err := a.speaker.Established(ctx)
	if err != nil {
		a.logger.Warn("读取 BGP 会话失败", "error", err)
	}
	for neighbor, up := range peers {
		a.metrics.established.WithLabelValues(neighbor).Set(boolGauge(up))
	}
	advertised, err := a.speaker.Advertised(ctx, peers)
	if err != nil {
		// 读不到就当作未发出：status 宁可报告未宣告，也不报告没有证实的宣告。
		a.logger.Warn("读取 adj-rib-out 失败", "error", err)
	}
	neighbors := make([]loadbalancer.NeighborStatus, 0, len(peers))
	for neighbor, up := range peers {
		neighbors = append(neighbors, loadbalancer.NeighborStatus{Address: neighbor, Established: up})
	}
	slices.SortFunc(neighbors, func(x, y loadbalancer.NeighborStatus) int { return strings.Compare(x.Address, y.Address) })
	vips := make([]loadbalancer.VIPStatus, 0, len(reasons))
	for vip, reason := range reasons {
		status := loadbalancer.VIPStatus{Address: vip, Announced: advertised[vip], Reason: reason}
		if status.Announced {
			status.Reason = ""
		} else if reason == "" {
			status.Reason = loadbalancer.NotAdvertised
		}
		vips = append(vips, status)
	}
	slices.SortFunc(vips, func(x, y loadbalancer.VIPStatus) int { return x.Address.Compare(y.Address) })
	a.mu.Lock()
	a.status.Neighbors, a.status.VIPs = neighbors, vips
	a.mu.Unlock()
}

// Status 返回最近一轮观察到的本机事实；首轮完成前各项为零值。
func (a *Agent) Status() loadbalancer.AgentStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

// StatusHandler 是只读的 GET /status（ADR-0018）：不含凭据，与 /metrics 同一明文监听。
func (a *Agent) StatusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(a.Status())
	})
}

func boolGauge(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
