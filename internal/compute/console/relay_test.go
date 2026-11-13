package console

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

var (
	td      = spiffeid.RequireTrustDomainFromString("test.example")
	proxyID = spiffeid.RequireFromPath(td, "/console-proxy")
	agentID = spiffeid.RequireFromPath(td, "/compute-agent/node1")
)

// ca 是测试用的 SPIFFE trust domain CA。
type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCA(t *testing.T) ca {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		URIs: []*url.URL{td.ID().URL()}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return ca{cert, key}
}

func (c ca) svid(t *testing.T, id spiffeid.ID) *x509svid.SVID {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs: []*url.URL{id.URL()}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &x509svid.SVID{ID: id, Certificates: []*x509.Certificate{cert}, PrivateKey: key}
}

func (c ca) bundle() *x509bundle.Bundle {
	return x509bundle.FromX509Authorities(td, []*x509.Certificate{c.cert})
}

// echo 是假的实例控制台：把写入的内容原样读回。
type echo struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func newEcho() echo { r, w := io.Pipe(); return echo{r, w} }

func (e echo) Read(p []byte) (int, error)  { return e.r.Read(p) }
func (e echo) Write(p []byte) (int, error) { return e.w.Write(p) }
func (e echo) Close() error                { _ = e.r.Close(); return e.w.Close() }

func startRelay(t *testing.T, c ca, open Opener) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	_ = ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, address, c.svid(t, agentID), c.bundle(), proxyID, Handler(open, slog.New(slog.DiscardHandler)))
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return address
}

// dial 以 id 的身份连接中继并发出升级请求，返回响应与连接。
func dial(t *testing.T, c ca, address string, id spiffeid.ID, path string) (*http.Response, *bufio.Reader, net.Conn, error) {
	t.Helper()
	config := tlsconfig.MTLSClientConfig(c.svid(t, id), c.bundle(), tlsconfig.AuthorizeID(agentID))
	var conn net.Conn
	var err error
	for range 50 { // Serve 在后台启动
		if conn, err = tls.Dial("tcp", address, config); err == nil || !strings.Contains(err.Error(), "connection refused") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn, "GET "+path+" HTTP/1.1\r\nHost: relay\r\nConnection: Upgrade\r\nUpgrade: "+Protocol+"\r\n\r\n"); err != nil {
		return nil, nil, nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	return resp, br, conn, err
}

func TestRelay(t *testing.T) {
	c := newCA(t)
	opened := make(chan [3]string, 1)
	address := startRelay(t, c, func(_ context.Context, ns, name, kind string) (io.ReadWriteCloser, error) {
		switch name {
		case "missing":
			return nil, ErrNotFound
		case "stopped":
			return nil, ErrNotRunning
		case "broken":
			return nil, errors.New("libvirt exploded")
		}
		opened <- [3]string{ns, name, kind}
		return newEcho(), nil
	})

	resp, br, conn, err := dial(t, c, address, proxyID, "/v1/consoles/default/vm1/serial")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Upgrade") != Protocol {
		t.Fatalf("upgrade: %s %v", resp.Status, resp.Header)
	}
	if got := <-opened; got != [3]string{"default", "vm1", "serial"} {
		t.Fatalf("opened %v", got)
	}
	if _, err := io.WriteString(conn, "hello"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("echo: %q %v", buf, err)
	}

	for path, want := range map[string]int{
		"/v1/consoles/default/missing/vnc":   http.StatusNotFound,
		"/v1/consoles/default/stopped/spice": http.StatusConflict,
		"/v1/consoles/default/broken/vnc":    http.StatusInternalServerError,
		"/v1/consoles/default/vm1/rdp":       http.StatusNotFound,
	} {
		resp, _, _, err := dial(t, c, address, proxyID, path)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Errorf("%s: %s, want %d", path, resp.Status, want)
		}
	}
}

func TestRelayRequiresUpgrade(t *testing.T) {
	c := newCA(t)
	address := startRelay(t, c, func(context.Context, string, string, string) (io.ReadWriteCloser, error) {
		t.Error("opened without upgrade")
		return newEcho(), nil
	})
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsconfig.MTLSClientConfig(c.svid(t, proxyID), c.bundle(), tlsconfig.AuthorizeID(agentID))}}
	var resp *http.Response
	var err error
	for range 50 {
		if resp, err = client.Get("https://" + address + "/v1/consoles/default/vm1/vnc"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("got %s", resp.Status)
	}
}

// 只有控制台代理的身份能连上；其他同 trust domain 的身份（例如别的节点的 Agent）在 TLS 握手时被拒绝。
func TestRelayRejectsOtherIdentities(t *testing.T) {
	c := newCA(t)
	address := startRelay(t, c, func(context.Context, string, string, string) (io.ReadWriteCloser, error) {
		t.Error("opened for an unauthorized peer")
		return newEcho(), nil
	})
	if _, _, _, err := dial(t, c, address, spiffeid.RequireFromPath(td, "/compute-agent/node2"), "/v1/consoles/default/vm1/serial"); err == nil {
		t.Fatal("other identity was accepted")
	}
	other := newCA(t) // 同名 trust domain，但不是 bundle 中的 CA
	config := tlsconfig.MTLSClientConfig(other.svid(t, proxyID), c.bundle(), tlsconfig.AuthorizeID(agentID))
	if conn, err := tls.Dial("tcp", address, config); err == nil {
		_, err = conn.Read(make([]byte, 1))
		_ = conn.Close()
		if err == nil {
			t.Fatal("certificate from another CA was accepted")
		}
	}
	if conn, err := tls.Dial("tcp", address, &tls.Config{InsecureSkipVerify: true}); err == nil { //nolint:gosec // 测试：没有客户端证书的连接
		_, err = conn.Read(make([]byte, 1))
		_ = conn.Close()
		if err == nil {
			t.Fatal("connection without a client certificate was accepted")
		}
	}
}

// 关闭中继（Agent 退出）时，已经建立的会话也结束，实例一侧被关闭。
func TestRelayStopEndsSessions(t *testing.T) {
	c := newCA(t)
	target := newEcho()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	_ = ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, address, c.svid(t, agentID), c.bundle(), proxyID,
			Handler(func(context.Context, string, string, string) (io.ReadWriteCloser, error) { return target, nil }, slog.New(slog.DiscardHandler)))
	}()
	resp, br, _, err := dial(t, c, address, proxyID, "/v1/consoles/default/vm1/vnc")
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("session still open after stop")
	}
	if _, err := target.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("instance side not closed: %v", err)
	}
}
