package cli

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// humanUser 是测试 issuer 登录的人在 apiserver 看来的用户名；nodeID 是测试 Workload API 发给调用方的 SPIFFE ID。
const humanUser, nodeID = "oidc:alice", "spiffe://test.example/node/a"

// workloadAPI 是只实现 FetchJWTSVID 的 SPIFFE Workload API：以 spire（代替 SPIRE 的 OIDC discovery）
// 的密钥签发 JWT-SVID。
type workloadAPI struct {
	workload.UnimplementedSpiffeWorkloadAPIServer
	spire *testIssuer
}

func (w workloadAPI) FetchJWTSVID(ctx context.Context, req *workload.JWTSVIDRequest) (*workload.JWTSVIDResponse, error) {
	if md, _ := metadata.FromIncomingContext(ctx); len(md.Get("workload.spiffe.io")) == 0 {
		return nil, status.Error(codes.InvalidArgument, "missing security header")
	}
	token, err := jwt.Signed(w.spire.signer).Claims(jwt.Claims{Issuer: w.spire.URL, Subject: nodeID,
		Audience: req.Audience, Expiry: jwt.NewNumericDate(time.Now().Add(5 * time.Minute))}).Serialize()
	if err != nil {
		return nil, err
	}
	return &workload.JWTSVIDResponse{Svids: []*workload.JWTSVID{{SpiffeId: nodeID, Svid: token}}}, nil
}

// startWorkloadAPI 在 unix socket 上运行 workloadAPI，返回 SPIFFE_ENDPOINT_SOCKET 的值。
func startWorkloadAPI(t *testing.T, spire *testIssuer) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	workload.RegisterSpiffeWorkloadAPIServer(s, workloadAPI{spire: spire})
	go func() { _ = s.Serve(l) }()
	t.Cleanup(s.Stop)
	return "unix://" + path
}

// assertWorkloadIdentity：没有保存的登录时用 SPIFFE 身份；有登录时人的登录优先；profile 指定 spiffe 时
// 只用 SPIFFE。logged 是已登录的 store。
func assertWorkloadIdentity(t *testing.T, ctx context.Context, logged Store, p Profile, spire *testIssuer) {
	t.Setenv("SPIFFE_ENDPOINT_SOCKET", startWorkloadAPI(t, spire))
	whoami := func(t *testing.T, s Store, p Profile) string {
		t.Helper()
		config, err := s.RESTConfig(ctx, "test", p)
		if err != nil {
			t.Fatal(err)
		}
		user, err := WhoAmI(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		return user.Username
	}
	if got := whoami(t, Store{Dir: t.TempDir()}, p); got != nodeID {
		t.Errorf("without a saved login: user %q, want %q", got, nodeID)
	}
	if got := whoami(t, logged, p); got != humanUser {
		t.Errorf("with a saved login: user %q, want oidc:alice", got)
	}
	pinned := p
	pinned.Auth = AuthSPIFFE
	if got := whoami(t, logged, pinned); got != nodeID {
		t.Errorf("auth pinned to spiffe: user %q, want %q", got, nodeID)
	}
}
