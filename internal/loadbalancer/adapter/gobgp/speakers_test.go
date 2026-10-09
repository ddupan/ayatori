package gobgp_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/adapter/gobgp"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/loadbalancer"
)

var vip = netip.MustParseAddr("10.62.0.10")

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

// startServer 在进程内运行真实的 GoBGP，开放 gRPC 并与对端建立 eBGP 会话。
func startServer(t *testing.T, asn uint32, routerID string, bgpPort, peerPort int, peerASN uint32, passive bool) string {
	t.Helper()
	grpcAddress := "127.0.0.1:" + strconv.Itoa(freePort(t))
	bgp := server.NewBgpServer(server.GrpcListenAddress(grpcAddress))
	go bgp.Serve()
	t.Cleanup(bgp.Stop)
	ctx := t.Context()
	if err := bgp.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn: asn, RouterId: routerID, ListenPort: int32(bgpPort), ListenAddresses: []string{"127.0.0.1"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := bgp.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: peerASN},
		Transport: &api.Transport{RemotePort: uint32(peerPort), PassiveMode: passive},
		Timers:    &api.Timers{Config: &api.TimersConfig{ConnectRetry: 1}},
	}}); err != nil {
		t.Fatal(err)
	}
	return grpcAddress
}

// received 返回对端（模拟 VyOS）是否从会话收到了该 VIP。
func received(t *testing.T, address string) bool {
	t.Helper()
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	stream, err := api.NewGoBgpServiceClient(connection).ListPath(t.Context(), &api.ListPathRequest{
		TableType: api.TableType_TABLE_TYPE_GLOBAL,
		Family:    &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST},
	})
	if err != nil {
		t.Fatal(err)
	}
	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range response.GetDestination().GetPaths() {
			if path.GetNeighborIp() != "" && path.GetNlri().GetPrefix().GetPrefix() == vip.String() {
				return true
			}
		}
	}
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if condition() {
			return
		}
	}
	t.Fatal("condition not met within 30s")
}

func TestSpeakersContract(t *testing.T) {
	speakerBGP, peerBGP := freePort(t), freePort(t)
	speaker := loadbalancer.Host{Name: "lb1", SpeakerAddress: startServer(t, 65001, "10.0.0.1", speakerBGP, peerBGP, 65000, false)}
	peer := startServer(t, 65000, "10.0.0.2", peerBGP, speakerBGP, 65001, true)
	speakers := &gobgp.Speakers{Credentials: insecure.NewCredentials()}
	t.Cleanup(speakers.Close)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	if announced, err := speakers.Announced(ctx, speaker, vip); err != nil || announced {
		t.Fatalf("宣告前 Announced = %v %v", announced, err)
	}
	for range 2 { // 重复宣告是幂等的
		if err := speakers.Announce(ctx, speaker, vip); err != nil {
			t.Fatal(err)
		}
	}
	if announced, err := speakers.Announced(ctx, speaker, vip); err != nil || !announced {
		t.Fatalf("宣告后 Announced = %v %v", announced, err)
	}
	eventually(t, func() bool { return received(t, peer) })

	for range 2 { // 重复撤回同样成功
		if err := speakers.Withdraw(ctx, speaker, vip); err != nil {
			t.Fatal(err)
		}
	}
	if announced, err := speakers.Announced(ctx, speaker, vip); err != nil || announced {
		t.Fatalf("撤回后 Announced = %v %v", announced, err)
	}
	eventually(t, func() bool { return !received(t, peer) })
}
