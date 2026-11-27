package ayatoriclient

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// ErrNoMachineIdentity：这台机器上没有可用的机器身份。
var ErrNoMachineIdentity = errors.New("no machine identity")

// defaultSPIFFESocket 是 homelab 节点上 spire-agent 的 Workload API；SPIFFE_ENDPOINT_SOCKET 优先。
const defaultSPIFFESocket = "/run/spire/agent-sockets/spire-agent.sock"

// MachineToken 是没有人参与时的凭据，audience 是环境的 clientID（ADR-0027）：
//  1. SPIFFE Workload API 的 JWT-SVID（节点、自动化、workload 集群中的组件）；
//  2. 实例元数据服务代领的 JWT-SVID（ADR-0016），在其身份端点实现后加在这里。
func MachineToken(ctx context.Context, p Profile) (string, error) {
	return SPIFFEToken(ctx, p)
}

// SPIFFEToken 从 Workload API 取 audience 为环境 clientID 的 JWT-SVID。
func SPIFFEToken(ctx context.Context, p Profile) (string, error) {
	addr := spiffeSocket()
	if addr == "" {
		return "", fmt.Errorf("%w: set SPIFFE_ENDPOINT_SOCKET or run spire-agent at %s", ErrNoMachineIdentity, defaultSPIFFESocket)
	}
	svid, err := workloadapi.FetchJWTSVID(ctx, jwtsvid.Params{Audience: p.ClientID}, workloadapi.WithAddr(addr))
	if err != nil {
		return "", fmt.Errorf("fetch JWT-SVID from %s: %w", addr, err)
	}
	return svid.Marshal(), nil
}

// spiffeSocket 返回 Workload API 地址：SPIFFE_ENDPOINT_SOCKET，或存在的默认 socket；都没有时为空。
func spiffeSocket() string {
	if addr := os.Getenv("SPIFFE_ENDPOINT_SOCKET"); addr != "" {
		return addr
	}
	if _, err := os.Stat(defaultSPIFFESocket); err == nil {
		return "unix://" + defaultSPIFFESocket
	}
	return ""
}
