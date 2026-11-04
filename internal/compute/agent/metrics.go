package agent

import (
	"slices"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"go.ddupan.top/ayatori/internal/compute/adapter/libvirt"
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
	domains     *domainStats
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
	m.domains = newDomainStats(domain)
	registry.MustRegister(m.libvirtUp, m.lastSuccess, m.shouldRun, m.running, m.poolBytes, m.domains)
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

// instanceStats 是一个实例的资源用量快照。
type instanceStats struct {
	namespace, name string
	libvirt.DomainStats
}

// domainStats 导出运行中实例的资源用量（ADR-0019 §6）。主循环每轮收敛后替换快照，抓取时只读快照，
// 不访问 libvirt 连接；实例停止或删除后随下一份快照消失。
type domainStats struct {
	snapshot                                         atomic.Pointer[[]instanceStats]
	cpu, memory, diskRead, diskWritten, netRx, netTx *prometheus.Desc
}

func newDomainStats(labels []string) *domainStats {
	device := append(slices.Clip(labels), "device")
	desc := func(name, help string, labels []string) *prometheus.Desc {
		return prometheus.NewDesc("ayatori_compute_agent_domain_"+name, help, labels, nil)
	}
	d := &domainStats{
		cpu:         desc("cpu_seconds_total", "实例累计使用的 CPU 时间。", labels),
		memory:      desc("memory_bytes", "balloon 报告的内存：current 为客户机当前可用，maximum 为配置量，rss 为宿主机上的驻留内存。", append(slices.Clip(labels), "type")),
		diskRead:    desc("disk_read_bytes_total", "磁盘累计读取的字节数。", device),
		diskWritten: desc("disk_written_bytes_total", "磁盘累计写入的字节数。", device),
		netRx:       desc("network_receive_bytes_total", "客户机网卡累计接收的字节数。", device),
		netTx:       desc("network_transmit_bytes_total", "客户机网卡累计发送的字节数。", device),
	}
	d.snapshot.Store(&[]instanceStats{})
	return d
}

func (d *domainStats) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{d.cpu, d.memory, d.diskRead, d.diskWritten, d.netRx, d.netTx} {
		ch <- desc
	}
}

func (d *domainStats) Collect(ch chan<- prometheus.Metric) {
	for _, s := range *d.snapshot.Load() {
		ch <- prometheus.MustNewConstMetric(d.cpu, prometheus.CounterValue, float64(s.CPUTime)/1e9, s.namespace, s.name)
		for kind, bytes := range s.Memory {
			ch <- prometheus.MustNewConstMetric(d.memory, prometheus.GaugeValue, float64(bytes), s.namespace, s.name, kind)
		}
		for _, io := range s.Disks {
			ch <- prometheus.MustNewConstMetric(d.diskRead, prometheus.CounterValue, float64(io.In), s.namespace, s.name, io.Device)
			ch <- prometheus.MustNewConstMetric(d.diskWritten, prometheus.CounterValue, float64(io.Out), s.namespace, s.name, io.Device)
		}
		for _, io := range s.Nics {
			ch <- prometheus.MustNewConstMetric(d.netRx, prometheus.CounterValue, float64(io.In), s.namespace, s.name, io.Device)
			ch <- prometheus.MustNewConstMetric(d.netTx, prometheus.CounterValue, float64(io.Out), s.namespace, s.name, io.Device)
		}
	}
}
