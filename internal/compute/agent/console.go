package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"

	"libvirt.org/go/libvirtxml"

	"go.ddupan.top/ayatori/internal/compute/adapter/libvirt"
	"go.ddupan.top/ayatori/internal/compute/agent/state"
	"go.ddupan.top/ayatori/internal/compute/console"
)

// consoleAlias 是中继的串口（ADR-0026）：CRD 默认带一个这样的串口，它的其余字段归用户。
const consoleAlias = "ua-ayatori-console"

// OpenConsole 打开实例的控制台，是 console.Opener。只服务本节点本地状态中的实例，并核对 libvirt 中同名 domain 的
// UUID：名字被别的 domain 占用时不会连过去。每个会话单独连接 libvirt，不与主循环共用连接。
//
// ponytail: 以"本节点 libvirt 中正在运行"判定实例所在；接入 DRBD（ADR-0020）后改为同时要求本节点是 Primary，
// 避免连到失去 Primary 后残留的 QEMU（PoC 05）。
func (a *Agent) OpenConsole(ctx context.Context, namespace, name, kind string) (io.ReadWriteCloser, error) {
	key := namespace + "/" + name
	records := a.store.List(kindDomain)
	i := slices.IndexFunc(records, func(r state.Record) bool { return r.Key == key && !r.Deleted })
	if i < 0 {
		return nil, console.ErrNotFound
	}
	hv, err := libvirt.Dial(a.config.LibvirtURI)
	if err != nil {
		return nil, err
	}
	target, err := openConsole(ctx, hv, domainName(namespace, name), records[i].UID, kind)
	if err != nil {
		_ = hv.Close()
		return nil, err
	}
	return &withClose{ReadWriteCloser: target, after: hv.Close}, nil
}

func openConsole(ctx context.Context, hv *libvirt.Client, name, uid, kind string) (io.ReadWriteCloser, error) {
	d, err := hv.Domain(name)
	if err != nil {
		return nil, err
	}
	if !d.Found || d.UUID != uid {
		return nil, console.ErrNotFound
	}
	if !d.Running() {
		return nil, console.ErrNotRunning
	}
	live, err := hv.LiveDomain(name)
	if err != nil {
		return nil, err
	}
	if kind == console.Serial {
		if !hasCharDevice(live, consoleAlias) {
			return nil, fmt.Errorf("%w: no serial with alias %s", console.ErrNoDevice, consoleAlias)
		}
		return hv.OpenConsole(name, consoleAlias)
	}
	path := graphicsSocket(live, kind)
	if path == "" {
		return nil, fmt.Errorf("%w: no %s socket", console.ErrNoDevice, kind)
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}

func hasCharDevice(d *libvirtxml.Domain, alias string) bool {
	if d.Devices == nil {
		return false
	}
	is := func(a *libvirtxml.DomainAlias) bool { return a != nil && a.Name == alias }
	return slices.ContainsFunc(d.Devices.Serials, func(s libvirtxml.DomainSerial) bool { return is(s.Alias) }) ||
		slices.ContainsFunc(d.Devices.Consoles, func(c libvirtxml.DomainConsole) bool { return is(c.Alias) })
}

// graphicsSocket 返回 live 配置中该种图形控制台的 unix socket；Agent 下发时只让它们监听 socket（prepareDomain）。
func graphicsSocket(d *libvirtxml.Domain, kind string) string {
	if d.Devices == nil {
		return ""
	}
	for _, g := range d.Devices.Graphics {
		var listeners []libvirtxml.DomainGraphicListener
		switch {
		case kind == console.VNC && g.VNC != nil:
			if g.VNC.Socket != "" {
				return g.VNC.Socket
			}
			listeners = g.VNC.Listeners
		case kind == console.SPICE && g.Spice != nil:
			listeners = g.Spice.Listeners
		}
		for _, l := range listeners {
			if l.Socket != nil && l.Socket.Socket != "" {
				return l.Socket.Socket
			}
		}
	}
	return ""
}

// withClose 在关闭控制台之后再关闭它的 libvirt 连接。只关闭一次：libvirt 的流释放两次会出错。
type withClose struct {
	io.ReadWriteCloser
	after func() error
	once  sync.Once
	err   error
}

func (w *withClose) Close() error {
	w.once.Do(func() { w.err = errors.Join(w.ReadWriteCloser.Close(), w.after()) })
	return w.err
}
