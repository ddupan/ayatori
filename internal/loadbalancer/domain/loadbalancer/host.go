package loadbalancer

// Host 是一台 LB 主机：一个 HAProxy（经 Data Plane API 管理）与一个 gobgpd。
type Host struct {
	Name           string
	ProxyURL       string
	SpeakerAddress string
}

// HostFacts 是本轮在某台 Host 上观察到的原始事实。
type HostFacts struct {
	Proxy ProxyFacts
	// Announced 表示该 Host 的 gobgpd 已宣告本 LoadBalancer 的 VIP。
	Announced bool
}

// HostAction 是聚合要求 Reconciler 在某台 Host 上执行的下一步；每轮每台 Host 至多一次写入。
type HostAction uint8

const (
	HostWait HostAction = iota
	ApplyConfig
	Announce
	Withdraw
	RemoveConfig
)

// AttachmentPhase 是 LoadBalancer 在某台 Host 上的落地进度，只呈现本轮观察，不作为下一轮依据。
type AttachmentPhase string

const (
	AttachmentPending     AttachmentPhase = "Pending"
	AttachmentConfigured  AttachmentPhase = "Configured"
	AttachmentAnnounced   AttachmentPhase = "Announced"
	AttachmentWithdrawn   AttachmentPhase = "Withdrawn"
	AttachmentUnavailable AttachmentPhase = "Unavailable"
)

type Attachment struct {
	Host    string
	Phase   AttachmentPhase
	Message string
}

// PlanHost 根据某台 Host 的事实决定下一步。observeErr 非空表示本轮无法观察该 Host，
// 只影响它自己的 Attachment。不变量：配置一致且 reload 成功后才宣告；删除时先撤回再删配置。
func (lb *LoadBalancer) PlanHost(host string, facts HostFacts, observeErr error, deleting bool) HostAction {
	switch {
	case observeErr != nil:
		lb.attach(host, AttachmentUnavailable, "无法观察主机："+observeErr.Error())
		return HostWait
	case deleting:
		return lb.planRemoval(host, facts)
	case !lb.snapshot.Address.IsValid():
		lb.attach(host, AttachmentPending, "等待 VIP")
		return HostWait
	case !facts.Proxy.Matches:
		// 已宣告时保留宣告：旧配置仍在服务，应用新配置期间不中断流量。
		lb.attach(host, AttachmentPending, "写入代理配置")
		return ApplyConfig
	case facts.Proxy.Reload == ReloadPending:
		lb.attach(host, AttachmentPending, "等待代理 reload")
		return HostWait
	case facts.Proxy.Reload == ReloadFailed:
		lb.attach(host, AttachmentUnavailable, "代理 reload 失败："+facts.Proxy.Message)
		return HostWait
	case !facts.Announced:
		lb.attach(host, AttachmentConfigured, "宣告 VIP")
		return Announce
	default:
		lb.attach(host, AttachmentAnnounced, "")
		return HostWait
	}
}

func (lb *LoadBalancer) planRemoval(host string, facts HostFacts) HostAction {
	switch {
	case facts.Announced:
		lb.attach(host, AttachmentAnnounced, "撤回 VIP")
		return Withdraw
	case facts.Proxy.Present:
		lb.attach(host, AttachmentConfigured, "删除代理配置")
		return RemoveConfig
	case facts.Proxy.Reload == ReloadPending:
		lb.attach(host, AttachmentConfigured, "等待代理 reload")
		return HostWait
	default:
		lb.attach(host, AttachmentWithdrawn, "")
		return HostWait
	}
}

// HostsReleased 表示本轮观察的全部 Host 均已撤回并删除配置，可以释放 VIP。
func (lb *LoadBalancer) HostsReleased() bool {
	for _, attachment := range lb.attachments {
		if attachment.Phase != AttachmentWithdrawn {
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
