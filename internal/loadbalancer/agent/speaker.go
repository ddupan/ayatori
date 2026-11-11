package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"

	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
)

// Speaker 是进程内的 GoBGP：本机唯一的 BGP speaker，只有本进程能增删路径，不开放 gRPC。
// 路径只存在于进程内，进程退出即会话断开；未开 graceful restart 时路由器随即撤回本机全部路径，
// 开启时路由器保留它们直到 restart time 到期或本机重连后发出 End-of-RIB。
type Speaker struct {
	server *server.BgpServer
	config Config
}

// StartSpeaker 启动 speaker 但不连接邻居：本地 RIB 先按 HAProxy 现状填好再 Connect，
// 否则重连后可能先发出空的 End-of-RIB，路由器随之删掉保留的路径。
//
// 全局 import 与 export 都只接受 VIP 段内的 /32、默认拒绝：export 是宣告的最后一道过滤；
// GoBGP 的全局 import 策略也作用于本机添加的路径，所以 import 不能是单纯的 reject。
// 非 route-server 模式下 GoBGP 忽略邻居级策略，必须用全局策略。
func StartSpeaker(ctx context.Context, config Config, logger *slog.Logger) (*Speaker, error) {
	level := &slog.LevelVar{}
	level.Set(slog.LevelWarn)
	s := server.NewBgpServer(server.LoggerOption(logger, level))
	go s.Serve()
	speaker := &Speaker{server: s, config: config}
	if err := speaker.configure(ctx, config); err != nil {
		s.Stop()
		return nil, err
	}
	return speaker, nil
}

func (s *Speaker) configure(ctx context.Context, config Config) error {
	if err := s.server.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn: config.ASN, RouterId: config.RouterID, ListenPort: -1,
	}}); err != nil {
		return fmt.Errorf("start BGP: %w", err)
	}
	if err := s.server.AddDefinedSet(ctx, &api.AddDefinedSetRequest{DefinedSet: &api.DefinedSet{
		DefinedType: api.DefinedType_DEFINED_TYPE_PREFIX, Name: "vip-pool",
		Prefixes: []*api.Prefix{{IpPrefix: config.VIPPool, MaskLengthMin: 32, MaskLengthMax: 32}},
	}}); err != nil {
		return fmt.Errorf("define VIP pool: %w", err)
	}
	if err := s.server.AddPolicy(ctx, &api.AddPolicyRequest{Policy: &api.Policy{Name: "vip-only", Statements: []*api.Statement{{
		Name:       "vip",
		Conditions: &api.Conditions{PrefixSet: &api.MatchSet{Type: api.MatchSet_TYPE_ANY, Name: "vip-pool"}},
		Actions:    &api.Actions{RouteAction: api.RouteAction_ROUTE_ACTION_ACCEPT},
	}}}}); err != nil {
		return fmt.Errorf("add policy: %w", err)
	}
	for _, direction := range []api.PolicyDirection{api.PolicyDirection_POLICY_DIRECTION_IMPORT, api.PolicyDirection_POLICY_DIRECTION_EXPORT} {
		if err := s.server.AddPolicyAssignment(ctx, &api.AddPolicyAssignmentRequest{Assignment: &api.PolicyAssignment{
			Name: "global", Direction: direction, Policies: []*api.Policy{{Name: "vip-only"}},
			DefaultAction: api.RouteAction_ROUTE_ACTION_REJECT,
		}}); err != nil {
			return fmt.Errorf("assign policy: %w", err)
		}
	}
	return nil
}

// Connect 建立到各邻居的会话。只主动连接（不监听 179）：路由器侧为 passive。
//
// 开启 graceful restart 时每次启动都以 restarting 身份连接：路由器若保留着上一个进程的路径，
// 会等本机的 End-of-RIB 再替换；GoBGP 在收到路由器的 End-of-RIB（或 deferral 到期）后才发出本机路径。
func (s *Speaker) Connect(ctx context.Context) error {
	restart := uint32(s.config.GracefulRestartTime.Seconds())
	for _, neighbor := range s.config.Neighbors {
		peer := &api.Peer{Conf: &api.PeerConf{NeighborAddress: neighbor.Address, PeerAsn: neighbor.ASN}}
		if neighbor.Port != 0 {
			peer.Transport = &api.Transport{RemotePort: neighbor.Port}
		}
		if restart > 0 {
			peer.GracefulRestart = &api.GracefulRestart{Enabled: true, RestartTime: restart, DeferralTime: 10, LocalRestarting: true}
			peer.AfiSafis = []*api.AfiSafi{{
				Config:            &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}, Enabled: true},
				MpGracefulRestart: &api.MpGracefulRestart{Config: &api.MpGracefulRestartConfig{Enabled: true}},
			}}
		}
		if err := s.server.AddPeer(ctx, &api.AddPeerRequest{Peer: peer}); err != nil {
			return fmt.Errorf("add neighbor %s: %w", neighbor.Address, err)
		}
	}
	return nil
}

// Sync 让本机宣告的路径恰为 want：与本地 RIB 比较，只增删差异。
func (s *Speaker) Sync(want []netip.Addr) error {
	have, err := s.Announced()
	if err != nil {
		return err
	}
	wanted := map[netip.Addr]bool{}
	for _, vip := range want {
		wanted[vip] = true
		if !have[vip] {
			if _, err := s.server.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{path(vip)}}); err != nil {
				return fmt.Errorf("announce %s: %w", vip, err)
			}
		}
	}
	for vip := range have {
		if !wanted[vip] {
			if err := s.server.DeletePath(apiutil.DeletePathRequest{Paths: []*apiutil.Path{path(vip)}}); err != nil {
				return fmt.Errorf("withdraw %s: %w", vip, err)
			}
		}
	}
	return nil
}

// Announced 返回本地 RIB 中本机添加的 /32；从对端学到的路径不算（import 策略本应全部拒绝）。
func (s *Speaker) Announced() (map[netip.Addr]bool, error) {
	announced := map[netip.Addr]bool{}
	err := s.server.ListPath(apiutil.ListPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: bgp.RF_IPv4_UC},
		func(nlri bgp.NLRI, paths []*apiutil.Path) {
			prefix, err := netip.ParsePrefix(nlri.String())
			if err != nil || prefix.Bits() != 32 {
				return
			}
			for _, p := range paths {
				if !p.PeerAddress.IsValid() {
					announced[prefix.Addr()] = true
				}
			}
		})
	return announced, err
}

// Advertised 返回出现在至少一个已建立会话 adj-rib-out 中的 /32：路径确实发给了路由器，
// 而不只是在本地 RIB 里（export 策略或会话状态都可能让它停在本机）。
func (s *Speaker) Advertised(ctx context.Context, established map[string]bool) (map[netip.Addr]bool, error) {
	advertised := map[netip.Addr]bool{}
	for neighbor, up := range established {
		if !up {
			continue
		}
		err := s.server.ListPath(apiutil.ListPathRequest{TableType: api.TableType_TABLE_TYPE_ADJ_OUT, Name: neighbor, Family: bgp.RF_IPv4_UC},
			func(nlri bgp.NLRI, _ []*apiutil.Path) {
				if prefix, err := netip.ParsePrefix(nlri.String()); err == nil && prefix.Bits() == 32 {
					advertised[prefix.Addr()] = true
				}
			})
		if err != nil {
			return nil, fmt.Errorf("list adj-rib-out of %s: %w", neighbor, err)
		}
	}
	return advertised, nil
}

// Established 返回各邻居的会话是否已建立。
func (s *Speaker) Established(ctx context.Context) (map[string]bool, error) {
	peers := map[string]bool{}
	err := s.server.ListPeer(ctx, &api.ListPeerRequest{}, func(peer *api.Peer) {
		peers[peer.GetConf().GetNeighborAddress()] = peer.GetState().GetSessionState() == api.PeerState_SESSION_STATE_ESTABLISHED
	})
	return peers, err
}

// Stop 断开会话。retain 且开启了 graceful restart 时不发 NOTIFICATION：按 RFC 4724，收到
// NOTIFICATION 的路由器会立即撤回本机路径，只有直接断开的会话才进入保留。
func (s *Speaker) Stop(retain bool) {
	if retain && s.config.GracefulRestartTime.Duration > 0 {
		if err := s.server.StopBgp(context.Background(), &api.StopBgpRequest{AllowGracefulRestart: true}); err != nil {
			slog.Error("停止 BGP 失败", "error", err)
		}
		return
	}
	s.server.Stop()
}

// path 以 0.0.0.0 为 next-hop，GoBGP 在 eBGP 宣告时改写为会话的本地地址。
func path(vip netip.Addr) *apiutil.Path {
	nlri, _ := bgp.NewIPAddrPrefix(netip.PrefixFrom(vip, 32))
	nextHop, _ := bgp.NewPathAttributeNextHop(netip.IPv4Unspecified())
	return &apiutil.Path{
		Family: bgp.RF_IPv4_UC,
		Nlri:   nlri,
		Attrs:  []bgp.PathAttributeInterface{bgp.NewPathAttributeOrigin(bgp.BGP_ORIGIN_ATTR_TYPE_IGP), nextHop},
	}
}
