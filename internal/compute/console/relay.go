// Package console 是 Agent 上的控制台中继（ADR-0016）：只接受控制台代理的 SPIFFE mTLS 连接，把它接到本机
// 实例的串口（libvirt console stream）或 VNC/SPICE 的 unix socket。节点不对外暴露任何控制台端口。
//
// 协议是 HTTP/1.1 升级：代理发 `GET /v1/consoles/<namespace>/<name>/<kind>`，带 `Connection: Upgrade` 与
// `Upgrade: ayatori-console`；中继回 101 后，连接上就是控制台的原始字节流。错误以普通 HTTP 状态返回。
package console

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// 控制台的种类。
const (
	Serial = "serial"
	VNC    = "vnc"
	SPICE  = "spice"
)

// Protocol 是 Upgrade 头的取值。
const Protocol = "ayatori-console"

var (
	// ErrNotFound：本节点没有这个实例（不归本节点，或 libvirt 中的同名 domain 不是它）。
	ErrNotFound = errors.New("instance not found on this node")
	// ErrNotRunning：实例在本节点上，但没有运行。
	ErrNotRunning = errors.New("instance not running")
	// ErrNoDevice：实例没有这种控制台。
	ErrNoDevice = errors.New("instance has no such console")
)

// Opener 打开实例的控制台。kind 是 Serial、VNC 或 SPICE 之一。
type Opener func(ctx context.Context, namespace, name, kind string) (io.ReadWriteCloser, error)

// Handler 返回中继的 HTTP handler。认证在 TLS 层完成，这里只做路由与转发。
func Handler(open Opener, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/consoles/{namespace}/{name}/{kind}", func(w http.ResponseWriter, r *http.Request) {
		ns, name, kind := r.PathValue("namespace"), r.PathValue("name"), r.PathValue("kind")
		if kind != Serial && kind != VNC && kind != SPICE {
			http.NotFound(w, r)
			return
		}
		if !headerHas(r.Header, "Connection", "upgrade") || !strings.EqualFold(r.Header.Get("Upgrade"), Protocol) {
			w.Header().Set("Upgrade", Protocol)
			http.Error(w, "upgrade to "+Protocol+" required", http.StatusUpgradeRequired)
			return
		}
		log := logger.With("namespace", ns, "name", name, "kind", kind, "peer", peerID(r))
		target, err := open(r.Context(), ns, name, kind)
		if err != nil {
			code := http.StatusInternalServerError
			switch {
			case errors.Is(err, ErrNotFound), errors.Is(err, ErrNoDevice):
				code = http.StatusNotFound
			case errors.Is(err, ErrNotRunning):
				code = http.StatusConflict
			}
			log.Info("console refused", "error", err)
			http.Error(w, err.Error(), code)
			return
		}
		defer func() { _ = target.Close() }()
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			log.Error("hijack", "error", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Time{})
		if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+Protocol+"\r\n\r\n"); err != nil {
			return
		}
		log.Info("console opened")
		splice(r.Context(), conn, buf.Reader, target)
		log.Info("console closed")
	})
	return mux
}

// splice 双向转发直到任一方结束或 ctx 取消，然后关闭两端，使另一方向的拷贝也返回。
func splice(ctx context.Context, conn net.Conn, in *bufio.Reader, target io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(target, in); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, target); done <- struct{}{} }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	_ = target.Close()
	_ = conn.Close()
	<-done
}

func headerHas(h http.Header, key, token string) bool {
	for _, v := range h.Values(key) {
		for t := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

func peerID(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	id, err := x509svid.IDFromCert(r.TLS.PeerCertificates[0])
	if err != nil {
		return ""
	}
	return id.String()
}

// Serve 在 address 上以 SPIFFE mTLS 提供 handler，只接受 proxy 这一个身份；ctx 取消时停止，并结束所有会话。
func Serve(ctx context.Context, address string, svid x509svid.Source, bundle x509bundle.Source, proxy spiffeid.ID, handler http.Handler) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           handler,
		TLSConfig:         tlsconfig.MTLSServerConfig(svid, bundle, tlsconfig.AuthorizeID(proxy)),
		ReadHeaderTimeout: 10 * time.Second,
		// 会话随 ctx 结束：Close 不管已经接管的连接。
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stop()
	err = server.Serve(tls.NewListener(listener, server.TLSConfig))
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
