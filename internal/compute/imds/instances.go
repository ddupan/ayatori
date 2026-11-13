// Package imds 是实例的元数据服务 ayatori-imds（ADR-0016）：拦截实例发往 169.254.169.254 的请求，
// 按请求进入的宿主机端口识别实例。它只依赖本机的信息，控制面不可用时照常响应。
package imds

import (
	"encoding/json"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
	"go.ddupan.top/ayatori/internal/compute/agent/state"
	"go.ddupan.top/ayatori/internal/compute/naming"
)

// Instance 是本节点上的一个实例。
type Instance struct {
	Kind      string
	Namespace string
	Name      string
	UID       string
	Node      string
	// UserData 是 cloud-init 的 user-data，经 NoCloud seed 提供（ADR-0016）。
	UserData string
}

// Port 是实例的一块网卡在宿主机上的端口。
type Port struct {
	Name     string
	Instance Instance
}

// readPorts 从 Compute Agent 的本地状态读出本节点实例的全部端口。本地状态只含分配给本节点的实例；
// Agent 以改名的方式写入记录，读到的总是完整的文件。
func readPorts(stateDir string) ([]Port, error) {
	store, err := state.Open(stateDir)
	if err != nil {
		return nil, err
	}
	var ports []Port
	add := func(inst Instance, n int) {
		for i := range n {
			ports = append(ports, Port{Name: naming.PortName(inst.UID, i), Instance: inst})
		}
	}
	for _, r := range store.List("LibvirtDomain") {
		var d computev1alpha1.LibvirtDomain
		if err := json.Unmarshal(r.Object, &d); err != nil {
			return nil, err
		}
		if !r.Deleted {
			add(Instance{Kind: r.Kind, Namespace: d.Namespace, Name: d.Name, UID: string(d.UID), Node: d.Spec.NodeName, UserData: d.Spec.UserData}, len(d.Spec.Interfaces))
		}
	}
	for _, r := range store.List("LXCContainer") {
		var c computev1alpha1.LXCContainer
		if err := json.Unmarshal(r.Object, &c); err != nil {
			return nil, err
		}
		if !r.Deleted {
			add(Instance{Kind: r.Kind, Namespace: c.Namespace, Name: c.Name, UID: string(c.UID), Node: c.Spec.NodeName, UserData: c.Spec.UserData}, len(c.Spec.Networks))
		}
	}
	return ports, nil
}
