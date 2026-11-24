package agent

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"go.ddupan.top/ayatori/internal/loadbalancer/domain/loadbalancer"
)

// listenerType 是 show stat 中 bind 行的 type；frontend 开启 socket-stats 后每个 bind 单独成行。
const listenerType = "3"

// Frontends 经 HAProxy stats socket 读取每个 bind 的地址与状态。bind 与其 frontend 都处于
// OPEN 才算 OPEN：被禁用的 frontend 可能仍保留监听。HAProxy 不应答时返回错误。
func Frontends(ctx context.Context, socket string) ([]loadbalancer.Frontend, error) {
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	defer func() { _ = connection.Close() }()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return nil, err
	}
	// 非交互模式：HAProxy 输出完整结果后关闭连接。
	if _, err := io.WriteString(connection, "show stat\n"); err != nil {
		return nil, err
	}
	return parseStat(connection)
}

func parseStat(r io.Reader) ([]loadbalancer.Frontend, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read show stat header: %w", err)
	}
	if len(header) == 0 || !strings.HasPrefix(header[0], "# ") {
		return nil, errors.New("show stat: unexpected header")
	}
	header[0] = strings.TrimPrefix(header[0], "# ")
	column := map[string]int{}
	for i, name := range header {
		column[name] = i
	}
	for _, name := range []string{"pxname", "svname", "status", "type", "addr"} {
		if _, ok := column[name]; !ok {
			return nil, fmt.Errorf("show stat: missing column %s", name)
		}
	}
	field := func(record []string, name string) string {
		if i := column[name]; i < len(record) {
			return record[i]
		}
		return ""
	}
	type bind struct {
		proxy   string
		address netip.Addr
		open    bool
	}
	var binds []bind
	frontendOpen := map[string]bool{}
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read show stat: %w", err)
		}
		if len(record) == 1 && record[0] == "" {
			continue
		}
		proxy, status := field(record, "pxname"), field(record, "status")
		if field(record, "svname") == "FRONTEND" {
			frontendOpen[proxy] = status == "OPEN"
			continue
		}
		if field(record, "type") != listenerType {
			continue
		}
		// unix socket 等非 IP 监听没有可宣告的地址。
		address, err := netip.ParseAddrPort(field(record, "addr"))
		if err != nil {
			continue
		}
		binds = append(binds, bind{proxy: proxy, address: address.Addr().Unmap(), open: status == "OPEN"})
	}
	frontends := make([]loadbalancer.Frontend, 0, len(binds))
	for _, b := range binds {
		frontends = append(frontends, loadbalancer.Frontend{Address: b.address, Open: b.open && frontendOpen[b.proxy]})
	}
	return frontends, nil
}
