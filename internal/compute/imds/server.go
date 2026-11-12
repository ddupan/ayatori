package imds

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.ddupan.top/ayatori/internal/compute/cloudinit"
)

const (
	tokenHeader    = "X-aws-ec2-metadata-token"             //nolint:gosec // header 名，不是凭据
	tokenTTLHeader = "X-aws-ec2-metadata-token-ttl-seconds" //nolint:gosec // header 名，不是凭据
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
	// 值来自实例名等用户输入，固定为纯文本，避免被当作 HTML 解析。
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/latest/api/token" {
		h.token(w, r)
		return
	}
	if file, ok := strings.CutPrefix(r.URL.Path, "/nocloud/"); ok {
		h.nocloud(w, r, file)
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
		_, _ = io.WriteString(w, "instance-id\nlocal-hostname\nkind\nnamespace\nname\nnode")
	case meta[key] != "":
		_, _ = io.WriteString(w, meta[key])
	default:
		http.NotFound(w, r)
	}
}

// nocloud 提供 cloud-init 的 NoCloud seed（ADR-0016）。cloud-init 以 HTTP 读取 seed 时不会先换取 token，这些路径因此
// 不要求 token：user-data 按约定不含密钥，响应仍受 IP TTL=1 与按端口识别实例的保护。vendor-data 为空；没有
// network-config，cloud-init 按默认在第一块网卡上用 DHCP。
func (h *handler) nocloud(w http.ResponseWriter, r *http.Request, file string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// user-data 是用户写的任意文本，固定为纯文本返回（ServeHTTP 已设置，这里写明，不依赖调用方）。
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	inst := h.instance.Load()
	switch file {
	case "meta-data":
		_, _ = io.WriteString(w, cloudinit.MetaData(inst.UID, inst.Name))
	case "user-data":
		_, _ = io.WriteString(w, inst.UserData)
	case "vendor-data":
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
	_, _ = io.WriteString(w, token)
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
