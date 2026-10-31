// Package libvirt 是 Compute Agent 访问本机 libvirt 的薄 adapter：只做调用与翻译，返回事实，
// 不含"何时下发、何时启动"的策略。经 RPC 连接 libvirtd，不需要 cgo。
package libvirt

import (
	"errors"
	"fmt"
	"net/url"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
)

type Client struct{ l *golibvirt.Libvirt }

// Dial 连接 address 上的 libvirtd 并打开 uri 指定的驱动。
// address 为 unix:///run/libvirt/libvirt-sock 或 tcp://host:port（测试用）。
func Dial(address, uri string) (*Client, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, err
	}
	var l *golibvirt.Libvirt
	switch u.Scheme {
	case "unix":
		l = golibvirt.NewWithDialer(dialers.NewLocal(dialers.WithSocket(u.Path), dialers.WithLocalTimeout(5*time.Second)))
	case "tcp":
		l = golibvirt.NewWithDialer(dialers.NewRemote(u.Hostname(), dialers.UsePort(u.Port()), dialers.WithRemoteTimeout(5*time.Second)))
	default:
		return nil, fmt.Errorf("unsupported libvirt address %q", address)
	}
	if err := l.ConnectToURI(golibvirt.ConnectURI(uri)); err != nil {
		return nil, err
	}
	return &Client{l: l}, nil
}

func (c *Client) Close() error { return c.l.Disconnect() }

// Disconnected 在连接断开后关闭；调用方据此重连。
func (c *Client) Disconnected() <-chan struct{} { return c.l.Disconnected() }

// Object 是 network 或 pool 的现状；XML 为 inactive（持久）配置。
type Object struct {
	Found  bool
	Active bool
	XML    string
}

func (c *Client) Network(name string) (Object, error) {
	n, err := c.l.NetworkLookupByName(name)
	if IsNotFound(err) {
		return Object{}, nil
	} else if err != nil {
		return Object{}, err
	}
	xml, err := inactiveXML(func(flags uint32) (string, error) { return c.l.NetworkGetXMLDesc(n, flags) }, uint32(golibvirt.NetworkXMLInactive))
	if err != nil {
		return Object{}, err
	}
	active, err := c.l.NetworkIsActive(n)
	return Object{Found: true, Active: active == 1, XML: xml}, err
}

func (c *Client) DefineNetwork(xml string) error {
	_, err := c.l.NetworkDefineXML(xml)
	return err
}

// StartNetwork 启动 network 并打开 autostart：network 是实例的前提，不应依赖 Agent 是否在运行。
func (c *Client) StartNetwork(name string) error {
	n, err := c.l.NetworkLookupByName(name)
	if err != nil {
		return err
	}
	if err := c.l.NetworkSetAutostart(n, 1); err != nil {
		return err
	}
	if active, err := c.l.NetworkIsActive(n); err != nil || active == 1 {
		return err
	}
	return c.l.NetworkCreate(n)
}

// RemoveNetwork 停止并删除定义；不存在时什么也不做。
func (c *Client) RemoveNetwork(name string) error {
	n, err := c.l.NetworkLookupByName(name)
	if IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if active, err := c.l.NetworkIsActive(n); err != nil {
		return err
	} else if active == 1 {
		if err := c.l.NetworkDestroy(n); err != nil {
			return err
		}
	}
	return c.l.NetworkUndefine(n)
}

func (c *Client) Pool(name string) (Object, error) {
	p, err := c.l.StoragePoolLookupByName(name)
	if IsNotFound(err) {
		return Object{}, nil
	} else if err != nil {
		return Object{}, err
	}
	xml, err := inactiveXML(func(flags uint32) (string, error) {
		return c.l.StoragePoolGetXMLDesc(p, golibvirt.StorageXMLFlags(flags))
	}, uint32(golibvirt.StorageXMLInactive))
	if err != nil {
		return Object{}, err
	}
	active, err := c.l.StoragePoolIsActive(p)
	return Object{Found: true, Active: active == 1, XML: xml}, err
}

func (c *Client) DefinePool(xml string) error {
	_, err := c.l.StoragePoolDefineXML(xml, 0)
	return err
}

// StartPool 启动 pool 并打开 autostart。不执行 pool-build：对 logical 等类型它会创建或覆盖底层存储。
func (c *Client) StartPool(name string) error {
	p, err := c.l.StoragePoolLookupByName(name)
	if err != nil {
		return err
	}
	if err := c.l.StoragePoolSetAutostart(p, 1); err != nil {
		return err
	}
	if active, err := c.l.StoragePoolIsActive(p); err != nil || active == 1 {
		return err
	}
	return c.l.StoragePoolCreate(p, 0)
}

// RemovePool 停止并删除定义，不删除底层存储。
func (c *Client) RemovePool(name string) error {
	p, err := c.l.StoragePoolLookupByName(name)
	if IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if active, err := c.l.StoragePoolIsActive(p); err != nil {
		return err
	} else if active == 1 {
		if err := c.l.StoragePoolDestroy(p); err != nil {
			return err
		}
	}
	return c.l.StoragePoolUndefine(p)
}

// Volume 返回 pool 中卷在本机的路径，以及它是否为块设备。
func (c *Client) Volume(pool, volume string) (path string, block bool, err error) {
	p, err := c.l.StoragePoolLookupByName(pool)
	if err != nil {
		return "", false, err
	}
	v, err := c.l.StorageVolLookupByName(p, volume)
	if err != nil {
		return "", false, err
	}
	kind, _, _, err := c.l.StorageVolGetInfo(v)
	if err != nil {
		return "", false, err
	}
	path, err = c.l.StorageVolGetPath(v)
	return path, golibvirt.StorageVolType(kind) == golibvirt.StorageVolBlock, err
}

// Domain 是实例的现状；State 与 Reason 与 `virsh domstate --reason` 的写法一致。
type Domain struct {
	Found  bool
	XML    string
	State  string
	Reason string
}

func (d Domain) Running() bool { return d.State == "running" || d.State == "paused" }

func (c *Client) Domain(name string) (Domain, error) {
	d, err := c.l.DomainLookupByName(name)
	if IsNotFound(err) {
		return Domain{}, nil
	} else if err != nil {
		return Domain{}, err
	}
	xml, err := c.l.DomainGetXMLDesc(d, golibvirt.DomainXMLInactive)
	if err != nil {
		return Domain{}, err
	}
	state, reason, err := c.l.DomainGetState(d, 0)
	if err != nil {
		return Domain{}, err
	}
	s, r := stateNames(golibvirt.DomainState(state), reason)
	return Domain{Found: true, XML: xml, State: s, Reason: r}, nil
}

// DefineDomain 定义实例并关闭 autostart：实例只由 Agent 或 drbd-reactor 启动（ADR-0015）。
func (c *Client) DefineDomain(xml string) error {
	d, err := c.l.DomainDefineXML(xml)
	if err != nil {
		return err
	}
	return c.l.DomainSetAutostart(d, 0)
}

func (c *Client) StartDomain(name string) error {
	d, err := c.l.DomainLookupByName(name)
	if err != nil {
		return err
	}
	return c.l.DomainCreate(d)
}

// RemoveDomain 强制停止并删除定义（连同 NVRAM），不删除磁盘。
func (c *Client) RemoveDomain(name string) error {
	d, err := c.l.DomainLookupByName(name)
	if IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	state, _, err := c.l.DomainGetState(d, 0)
	if err != nil {
		return err
	}
	if golibvirt.DomainState(state) != golibvirt.DomainShutoff {
		if err := c.l.DomainDestroy(d); err != nil {
			return err
		}
	}
	err = c.l.DomainUndefineFlags(d, golibvirt.DomainUndefineNvram)
	if isUnsupportedFlags(err) { // test 驱动不支持 NVRAM flag；QEMU 驱动支持
		err = c.l.DomainUndefine(d)
	}
	return err
}

// NodeInfo 返回本机的 CPU 数与内存（字节）。
func (c *Client) NodeInfo() (cpus int64, memory int64, err error) {
	_, memKiB, n, _, _, _, _, _, err := c.l.NodeGetInfo()
	return int64(n), int64(memKiB) * 1024, err
}

func stateNames(state golibvirt.DomainState, reason int32) (string, string) {
	switch state {
	case golibvirt.DomainRunning:
		return "running", pick(runningReasons, reason)
	case golibvirt.DomainShutoff:
		return "shut off", pick(shutoffReasons, reason)
	case golibvirt.DomainPaused:
		return "paused", "unknown"
	case golibvirt.DomainShutdown:
		return "in shutdown", "unknown"
	case golibvirt.DomainCrashed:
		return "crashed", "unknown"
	case golibvirt.DomainPmsuspended:
		return "pmsuspended", "unknown"
	case golibvirt.DomainBlocked:
		return "idle", "unknown"
	}
	return "no state", "unknown"
}

var (
	runningReasons = []string{"unknown", "booted", "migrated", "restored", "from snapshot", "unpaused",
		"migration canceled", "save canceled", "event wakeup", "crashed", "post-copy", "post-copy failed"}
	shutoffReasons = []string{"unknown", "shutdown", "destroyed", "crashed", "migrated", "saved", "failed",
		"from snapshot", "daemon"}
)

func pick(names []string, i int32) string {
	if i < 0 || int(i) >= len(names) {
		return "unknown"
	}
	return names[i]
}

// IsNotFound 判断对象不存在。go-libvirt 自带的 IsNotFound 只认 domain，这里覆盖 network、pool 与卷。
func IsNotFound(err error) bool {
	var e golibvirt.Error
	if !errors.As(err, &e) {
		return false
	}
	switch golibvirt.ErrorNumber(e.Code) {
	case golibvirt.ErrNoDomain, golibvirt.ErrNoNetwork, golibvirt.ErrNoStoragePool, golibvirt.ErrNoStorageVol:
		return true
	}
	return false
}

// inactiveXML 读取持久配置。libvirt 的 test 驱动不支持 network 与 pool 的 INACTIVE flag，
// 此时退回不带 flag 读取：test 驱动中两者相同；QEMU 驱动支持该 flag，不会走到这里。
func inactiveXML(get func(flags uint32) (string, error), inactive uint32) (string, error) {
	xml, err := get(inactive)
	if isUnsupportedFlags(err) {
		return get(0)
	}
	return xml, err
}

func isUnsupportedFlags(err error) bool {
	var e golibvirt.Error
	return errors.As(err, &e) && golibvirt.ErrorNumber(e.Code) == golibvirt.ErrInvalidArg
}
