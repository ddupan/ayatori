package agent

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// 指标供中央 VictoriaMetrics 直接抓取，告警不经过控制面（ADR-0015 可观测性、ADR-0019 §6）。实例名用
// name 标签而不是 instance：后者是抓取目标标签，重名会被改写（PoC 06）。
// nameLabel 是对象名的标签。
const nameLabel = "name"

type metrics struct {
	libvirtUp   prometheus.Gauge
	lastSuccess *prometheus.GaugeVec
	shouldRun   *prometheus.GaugeVec
	running     *prometheus.GaugeVec
	poolBytes   *prometheus.GaugeVec
}

func newMetrics(registry prometheus.Registerer) *metrics {
	object := []string{"kind", "namespace", nameLabel}
	domain := object[1:]
	m := &metrics{
		libvirtUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ayatori_compute_agent_libvirt_up", Help: "本轮能否连接 libvirt。"}),
		lastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ayatori_compute_agent_object_last_success_timestamp_seconds",
			Help: "对象最近一次成功收敛到 libvirt 的时间；从未成功为 0。"}, object),
		shouldRun: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ayatori_compute_agent_domain_should_run",
			Help: "实例是否应当运行：runState 不是 Stopped，且没有在客户机内关机或被 destroy。"}, domain),
		running: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ayatori_compute_agent_domain_running", Help: "实例是否正在本节点运行（含暂停）。"}, domain),
		poolBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ayatori_compute_agent_pool_bytes", Help: "libvirt storage pool 的容量与可用空间。"},
			[]string{nameLabel, "type"}),
	}
	registry.MustRegister(m.libvirtUp, m.lastSuccess, m.shouldRun, m.running, m.poolBytes)
	return m
}

type objectID struct{ kind, namespace, name string }

// observe 记录一轮中各对象的结果；不再出现的对象删除其全部指标。
func (m *metrics) observe(succeeded map[objectID]time.Time, seen, previous map[objectID]bool) {
	for id := range seen {
		var t float64
		if at, ok := succeeded[id]; ok {
			t = float64(at.Unix())
		}
		m.lastSuccess.WithLabelValues(id.kind, id.namespace, id.name).Set(t)
	}
	for id := range previous {
		if seen[id] {
			continue
		}
		delete(succeeded, id)
		m.lastSuccess.DeleteLabelValues(id.kind, id.namespace, id.name)
		switch id.kind {
		case kindDomain:
			m.shouldRun.DeleteLabelValues(id.namespace, id.name)
			m.running.DeleteLabelValues(id.namespace, id.name)
		case kindPool:
			m.poolBytes.DeletePartialMatch(prometheus.Labels{nameLabel: id.name})
		}
	}
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
