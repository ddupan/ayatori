// Package agent 是 LB 主机上的 LB agent（ADR-0014）：按本机 HAProxy 的 frontend 状态，经进程内
// GoBGP 宣告或撤回 VIP /32。无状态：每轮从 HAProxy 现状计算应宣告集合，不依赖 controller。
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

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
}

type Neighbor struct {
	Address string `json:"address"`
	ASN     uint32 `json:"asn"`
	// Port 为零时使用 179；只供测试改写。
	Port uint32 `json:"port,omitempty"`
}

// LoadConfig 读取并校验配置；缺省间隔为 2 秒。
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
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
	metrics *metrics
	logger  *slog.Logger
}

func New(ctx context.Context, config Config, registry prometheus.Registerer, logger *slog.Logger) (*Agent, error) {
	pool, err := config.pool()
	if err != nil {
		return nil, err
	}
	speaker, err := StartSpeaker(ctx, config, logger)
	if err != nil {
		return nil, err
	}
	return &Agent{config: config, pool: pool, speaker: speaker, metrics: newMetrics(registry), logger: logger}, nil
}

func (a *Agent) Run(ctx context.Context) {
	defer a.speaker.Stop()
	ticker := time.NewTicker(a.config.Interval.Duration)
	defer ticker.Stop()
	for {
		a.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
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
	want := loadbalancer.Announcements(a.pool, frontends)
	if err := a.speaker.Sync(want); err != nil {
		a.logger.Error("收敛宣告失败", "error", err)
	} else {
		a.metrics.lastSuccess.SetToCurrentTime()
	}
	a.observe(ctx)
}

// observe 以本地 RIB 为准导出指标，而不是以本轮期望为准。
func (a *Agent) observe(ctx context.Context) {
	announced, err := a.speaker.Announced()
	if err == nil {
		a.metrics.announced.Reset()
		for vip := range announced {
			a.metrics.announced.WithLabelValues(vip.String()).Set(1)
		}
	}
	if peers, err := a.speaker.Established(ctx); err == nil {
		for neighbor, up := range peers {
			a.metrics.established.WithLabelValues(neighbor).Set(boolGauge(up))
		}
	}
}

func boolGauge(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
