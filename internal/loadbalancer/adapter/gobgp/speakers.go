// Package gobgp 经 gobgpd gRPC API 宣告与撤回 VIP /32，只返回事实。
//
// 部署契约（由 homelab-infra 保证）：gobgpd 只由 Ayatori 添加 VIP 路径，与 VyOS 的会话及
// 策略为静态配置；systemd 以 BindsTo=haproxy.service 让 HAProxy 停止时会话随之断开。
// gobgpd 不持久化路径，重启后由 Reconciler 观察到缺失再宣告。
package gobgp

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"sync"

	api "github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/loadbalancer"
)

// Speakers 对任意 Speaker 执行同一套操作；Credentials 负责 mTLS。
type Speakers struct {
	Credentials credentials.TransportCredentials

	mu          sync.Mutex
	connections map[string]*grpc.ClientConn
}

var ipv4Unicast = &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}

// Announced 返回 Speaker 的全局 RIB 中是否已有该 VIP 的本地路径。
func (s *Speakers) Announced(ctx context.Context, host loadbalancer.Host, vip netip.Addr) (bool, error) {
	client, err := s.client(host)
	if err != nil {
		return false, err
	}
	stream, err := client.ListPath(ctx, &api.ListPathRequest{
		TableType: api.TableType_TABLE_TYPE_GLOBAL,
		Family:    ipv4Unicast,
		Prefixes:  []*api.TableLookupPrefix{{Prefix: vip.String() + "/32", Type: api.TableLookupPrefix_TYPE_EXACT}},
	})
	if err != nil {
		return false, err
	}
	found := false
	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return found, nil
		}
		if err != nil {
			return false, err
		}
		for _, path := range response.GetDestination().GetPaths() {
			// 本机经 API 安装的路径没有对端地址；从对端学到的同前缀路径不算本机宣告。
			found = found || path.GetNeighborIp() == ""
		}
	}
}

// Announce 宣告 VIP /32；重复宣告同一前缀只会替换原路径。
func (s *Speakers) Announce(ctx context.Context, host loadbalancer.Host, vip netip.Addr) error {
	client, err := s.client(host)
	if err != nil {
		return err
	}
	_, err = client.AddPath(ctx, &api.AddPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Path: path(vip)})
	return err
}

// Withdraw 撤回 VIP /32；路径不存在时同样成功。
func (s *Speakers) Withdraw(ctx context.Context, host loadbalancer.Host, vip netip.Addr) error {
	client, err := s.client(host)
	if err != nil {
		return err
	}
	_, err = client.DeletePath(ctx, &api.DeletePathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: ipv4Unicast, Path: path(vip)})
	return err
}

// Close 释放到各 Speaker 的连接；在 manager 停止后调用。
func (s *Speakers) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for address, connection := range s.connections {
		_ = connection.Close()
		delete(s.connections, address)
	}
}

// path 以 0.0.0.0 为 next-hop，由 gobgpd 在宣告时改写为会话的本地地址。
func path(vip netip.Addr) *api.Path {
	return &api.Path{
		Family: ipv4Unicast,
		Nlri:   &api.NLRI{Nlri: &api.NLRI_Prefix{Prefix: &api.IPAddressPrefix{Prefix: vip.String(), PrefixLen: 32}}},
		Pattrs: []*api.Attribute{
			{Attr: &api.Attribute_Origin{Origin: &api.OriginAttribute{Origin: 0}}},
			{Attr: &api.Attribute_NextHop{NextHop: &api.NextHopAttribute{NextHop: "0.0.0.0"}}},
		},
	}
}

func (s *Speakers) client(host loadbalancer.Host) (api.GoBgpServiceClient, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if connection, ok := s.connections[host.SpeakerAddress]; ok {
		return api.NewGoBgpServiceClient(connection), nil
	}
	connection, err := grpc.NewClient(host.SpeakerAddress, grpc.WithTransportCredentials(s.Credentials))
	if err != nil {
		return nil, err
	}
	if s.connections == nil {
		s.connections = map[string]*grpc.ClientConn{}
	}
	s.connections[host.SpeakerAddress] = connection
	return api.NewGoBgpServiceClient(connection), nil
}
