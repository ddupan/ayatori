package imds

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	tokenHeader    = "X-aws-ec2-metadata-token"
	tokenTTLHeader = "X-aws-ec2-metadata-token-ttl-seconds"
	maxTokenTTL    = 6 * time.Hour
)

// handler 是一个端口专属的元数据服务：照搬 AWS IMDSv2，先 PUT 换取 token，GET 必须带 token
// （ADR-0016 防 SSRF）。token 只在该端口有效，因此绑定实例。
type handler struct {
	instance atomic.Pointer[Instance]
	now      func() time.Time

	mu     sync.Mutex
	tokens map[string]time.Time
}

func newHandler(inst Instance) *handler {
	h := &handler{now: time.Now, tokens: map[string]time.Time{}}
	h.instance.Store(&inst)
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/latest/api/token" {
		h.token(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.valid(r.Header.Get(tokenHeader)) {
		http.Error(w, "valid token required", http.StatusUnauthorized)
		return
	}
	inst := h.instance.Load()
	meta := map[string]string{
		"instance-id":    inst.UID,
		"local-hostname": inst.Name,
		"kind":           inst.Kind,
		"namespace":      inst.Namespace,
		"name":           inst.Name,
		"node":           inst.Node,
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/latest/meta-data/")
	switch {
	case !ok:
		http.NotFound(w, r)
	case key == "":
		_, _ = fmt.Fprint(w, "instance-id\nlocal-hostname\nkind\nnamespace\nname\nnode")
	case meta[key] != "":
		_, _ = fmt.Fprint(w, meta[key])
	default:
		http.NotFound(w, r)
	}
}

// token 签发会话 token。带 X-Forwarded-For 的请求来自代理，拒绝，与 IMDSv2 相同。
func (h *handler) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("X-Forwarded-For") != "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	seconds, err := strconv.Atoi(r.Header.Get(tokenTTLHeader))
	if err != nil || seconds < 1 || time.Duration(seconds)*time.Second > maxTokenTTL {
		http.Error(w, tokenTTLHeader+" must be 1-21600", http.StatusBadRequest)
		return
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	now := h.now()
	h.mu.Lock()
	for t, exp := range h.tokens {
		if !now.Before(exp) {
			delete(h.tokens, t)
		}
	}
	h.tokens[token] = now.Add(time.Duration(seconds) * time.Second)
	h.mu.Unlock()
	w.Header().Set(tokenTTLHeader, strconv.Itoa(seconds))
	_, _ = fmt.Fprint(w, token)
}

func (h *handler) valid(token string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	exp, ok := h.tokens[token]
	return ok && h.now().Before(exp)
}

// listen 在 Address:port 上监听，并在监听 socket 上设置 IP_TTL=1：回包（包括 SYN-ACK）出不了第一跳，
// 实例内嵌套的容器或被转发的请求连握手都完成不了（PoC 04）。
func listen(port int) (net.Listener, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) { serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TTL, 1) }); err != nil { //nolint:gosec // fd 来自内核
			return err
		}
		return serr
	}}
	return lc.Listen(context.Background(), "tcp4", net.JoinHostPort(Address, strconv.Itoa(port)))
}
