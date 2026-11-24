package agent

import (
	"testing"

	"go.ddupan.top/ayatori/internal/loadbalancer/domain/loadbalancer"
)

func TestNodeStatus(t *testing.T) {
	up := loadbalancer.NeighborStatus{Address: "10.60.0.1", Established: true}
	down := loadbalancer.NeighborStatus{Address: "10.60.0.2"}
	for _, c := range []struct {
		status loadbalancer.AgentStatus
		ready  bool
		reason string
	}{
		{loadbalancer.AgentStatus{Neighbors: []loadbalancer.NeighborStatus{up}}, false, reasonHAProxyDown},
		{loadbalancer.AgentStatus{HAProxyUp: true, Neighbors: []loadbalancer.NeighborStatus{down}}, false, reasonNoBGPSession},
		{loadbalancer.AgentStatus{HAProxyUp: true}, false, reasonNoBGPSession},
		{loadbalancer.AgentStatus{HAProxyUp: true, Neighbors: []loadbalancer.NeighborStatus{down, up}}, true, reasonAgentReady},
	} {
		if got := nodeStatus(c.status); got.Ready != c.ready || got.Reason != c.reason {
			t.Errorf("nodeStatus(%+v) = %+v, want ready=%v reason=%s", c.status, got, c.ready, c.reason)
		}
	}
}
