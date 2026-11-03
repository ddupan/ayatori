// Package agentstatus 读取 LB agent 只读的 GET /status（ADR-0018）。
package agentstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/loadbalancer"
)

// maxBody 限制读取的应答大小：接口无认证，伪造或异常的应答不应耗尽 controller 内存。
const maxBody = 1 << 20

// Client 以明文或 HTTPS 访问 agent；接口不含凭据，伪造的应答只会让 status 失真，不影响路由。
type Client struct {
	HTTP *http.Client
}

func (c Client) Status(ctx context.Context, agentURL string) (loadbalancer.AgentStatus, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(agentURL, "/")+"/status", nil)
	if err != nil {
		return loadbalancer.AgentStatus{}, err
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return loadbalancer.AgentStatus{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return loadbalancer.AgentStatus{}, fmt.Errorf("agent status: HTTP %d", response.StatusCode)
	}
	var status loadbalancer.AgentStatus
	if err := json.NewDecoder(io.LimitReader(response.Body, maxBody)).Decode(&status); err != nil {
		return loadbalancer.AgentStatus{}, fmt.Errorf("decode agent status: %w", err)
	}
	return status, nil
}
