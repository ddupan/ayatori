package loadbalancer

// Host 是一台 LB 主机上经 Data Plane API 管理的 HAProxy。路由由该主机上的 LB agent 自行宣告
// （ADR-0014），controller 不连接 BGP speaker。
type Host struct {
	Name     string
	ProxyURL string
}

// HostAction 是聚合要求 Reconciler 在某台 Host 上执行的下一步；每轮每台 Host 至多一次写入。
type HostAction uint8

const (
	HostWait HostAction = iota
	ApplyConfig
	RemoveConfig
)

// AttachmentPhase 是 LoadBalancer 在某台 Host 上的落地进度，只呈现本轮观察，不作为下一轮依据。
type AttachmentPhase string

const (
	AttachmentPending AttachmentPhase = "Pending"
	// AttachmentConfigured 表示配置已生效，但不满足宣告规则，该主机的 agent 不宣告。
	AttachmentConfigured AttachmentPhase = "Configured"
	// AttachmentReady 表示配置已生效且满足宣告规则，该主机的 agent 会宣告 VIP。
	AttachmentReady       AttachmentPhase = "Ready"
	AttachmentRemoved     AttachmentPhase = "Removed"
	AttachmentUnavailable AttachmentPhase = "Unavailable"
)

type Attachment struct {
	Host    string
	Phase   AttachmentPhase
	Message string
}

// PlanHost 根据某台 Host 的事实决定下一步。observeErr 非空表示本轮无法观察该 Host，
// 只影响它自己的 Attachment。宣告与撤回由 agent 按同一规则（Announcements）在本机完成。
func (lb *LoadBalancer) PlanHost(host string, facts ProxyFacts, observeErr error, deleting bool) HostAction {
	switch {
	case observeErr != nil:
		lb.attach(host, AttachmentUnavailable, "无法观察主机："+observeErr.Error())
		return HostWait
	case deleting:
		return lb.planRemoval(host, facts)
	case !lb.snapshot.Address.IsValid():
		lb.attach(host, AttachmentPending, "等待 VIP")
		return HostWait
	case !facts.Matches:
		// 旧配置在写入新配置期间继续服务，agent 的宣告不受影响。
		lb.attach(host, AttachmentPending, "写入代理配置")
		return ApplyConfig
	case facts.Reload == ReloadPending:
		lb.attach(host, AttachmentPending, "等待代理 reload")
		return HostWait
	case facts.Reload == ReloadFailed:
		lb.attach(host, AttachmentUnavailable, "代理 reload 失败："+facts.Message)
		return HostWait
	case !announces(lb.snapshot.Address, facts.Frontends):
		lb.attach(host, AttachmentConfigured, "frontend 未处于 OPEN，主机不宣告")
		return HostWait
	default:
		lb.attach(host, AttachmentReady, "")
		return HostWait
	}
}

// planRemoval 直接删除配置：frontend 消失后 agent 在下一轮自行撤回，无需先撤回。
func (lb *LoadBalancer) planRemoval(host string, facts ProxyFacts) HostAction {
	switch {
	case facts.Present:
		lb.attach(host, AttachmentConfigured, "删除代理配置")
		return RemoveConfig
	case facts.Reload == ReloadPending:
		lb.attach(host, AttachmentConfigured, "等待代理 reload")
		return HostWait
	default:
		lb.attach(host, AttachmentRemoved, "")
		return HostWait
	}
}

// HostsReleased 表示本轮观察的全部 Host 均已删除配置，可以释放 VIP。
func (lb *LoadBalancer) HostsReleased() bool {
	for _, attachment := range lb.attachments {
		if attachment.Phase != AttachmentRemoved {
			return false
		}
	}
	return true
}

func (lb *LoadBalancer) Attachments() []Attachment { return lb.attachments }

func (lb *LoadBalancer) attach(host string, phase AttachmentPhase, message string) {
	lb.attachments = append(lb.attachments, Attachment{Host: host, Phase: phase, Message: message})
}

// HostFailed 记录本轮在某台 Host 上执行的写入失败；结果由下一轮重新观察确定。
func (lb *LoadBalancer) HostFailed(host string, err error) {
	for i := range lb.attachments {
		if lb.attachments[i].Host == host {
			lb.attachments[i].Phase, lb.attachments[i].Message = AttachmentUnavailable, "写入失败："+err.Error()
		}
	}
}
