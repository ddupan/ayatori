// Package libvirt 是 Compute Agent 访问本机 libvirt 的薄 adapter：只做调用与翻译，返回事实，
// 不含"何时下发、何时启动"的策略。使用 libvirt 官方的 cgo 绑定。
package libvirt

import (
	"errors"

	golibvirt "libvirt.org/go/libvirt"
)

type Client struct{ conn *golibvirt.Connect }

// Dial 打开 uri（例如 qemu:///system；测试用 test:///default）。
func Dial(uri string) (*Client, error) {
	conn, err := golibvirt.NewConnect(uri)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn}, nil
}

func (c *Client) Close() error {
	_, err := c.conn.Close()
	return err
}

// Alive 报告连接是否仍可用；断开后调用方重连。
func (c *Client) Alive() bool {
	ok, err := c.conn.IsAlive()
	return err == nil && ok
}

// Object 是 network 或 pool 的现状；XML 为 inactive（持久）配置。
type Object struct {
	Found  bool
	Active bool
	XML    string
}

func (c *Client) Network(name string) (Object, error) {
	n, err := c.conn.LookupNetworkByName(name)
	if IsNotFound(err) {
		return Object{}, nil
	} else if err != nil {
		return Object{}, err
	}
	defer n.Free()
	xml, err := inactiveXML(func(inactive bool) (string, error) {
		if inactive {
			return n.GetXMLDesc(golibvirt.NETWORK_XML_INACTIVE)
		}
		return n.GetXMLDesc(0)
	})
	if err != nil {
		return Object{}, err
	}
	active, err := n.IsActive()
	return Object{Found: true, Active: active, XML: xml}, err
}

func (c *Client) DefineNetwork(xml string) error {
	n, err := c.conn.NetworkDefineXML(xml)
	if err != nil {
		return err
	}
	return n.Free()
}

// StartNetwork 启动 network 并打开 autostart：network 是实例的前提，不应依赖 Agent 是否在运行。
func (c *Client) StartNetwork(name string) error {
	n, err := c.conn.LookupNetworkByName(name)
	if err != nil {
		return err
	}
	defer n.Free()
	if err := n.SetAutostart(true); err != nil {
		return err
	}
	if active, err := n.IsActive(); err != nil || active {
		return err
	}
	return n.Create()
}

// RemoveNetwork 停止并删除定义；不存在时什么也不做。
func (c *Client) RemoveNetwork(name string) error {
	n, err := c.conn.LookupNetworkByName(name)
	if IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	defer n.Free()
	if active, err := n.IsActive(); err != nil {
		return err
	} else if active {
		if err := n.Destroy(); err != nil {
			return err
		}
	}
	return n.Undefine()
}

func (c *Client) Pool(name string) (Object, error) {
	p, err := c.conn.LookupStoragePoolByName(name)
	if IsNotFound(err) {
		return Object{}, nil
	} else if err != nil {
		return Object{}, err
	}
	defer p.Free()
	xml, err := inactiveXML(func(inactive bool) (string, error) {
		if inactive {
			return p.GetXMLDesc(golibvirt.STORAGE_XML_INACTIVE)
		}
		return p.GetXMLDesc(0)
	})
	if err != nil {
		return Object{}, err
	}
	active, err := p.IsActive()
	return Object{Found: true, Active: active, XML: xml}, err
}

func (c *Client) DefinePool(xml string) error {
	p, err := c.conn.StoragePoolDefineXML(xml, 0)
	if err != nil {
		return err
	}
	return p.Free()
}

// StartPool 启动 pool 并打开 autostart。不执行 pool-build：对 logical 等类型它会创建或覆盖底层存储。
func (c *Client) StartPool(name string) error {
	p, err := c.conn.LookupStoragePoolByName(name)
	if err != nil {
		return err
	}
	defer p.Free()
	if err := p.SetAutostart(true); err != nil {
		return err
	}
	if active, err := p.IsActive(); err != nil || active {
		return err
	}
	return p.Create(0)
}

// RemovePool 停止并删除定义，不删除底层存储。
func (c *Client) RemovePool(name string) error {
	p, err := c.conn.LookupStoragePoolByName(name)
	if IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	defer p.Free()
	if active, err := p.IsActive(); err != nil {
		return err
	} else if active {
		if err := p.Destroy(); err != nil {
			return err
		}
	}
	return p.Undefine()
}

// Volume 返回 pool 中卷在本机的路径，以及它是否为块设备。
func (c *Client) Volume(pool, volume string) (path string, block bool, err error) {
	p, err := c.conn.LookupStoragePoolByName(pool)
	if err != nil {
		return "", false, err
	}
	defer p.Free()
	v, err := p.LookupStorageVolByName(volume)
	if err != nil {
		return "", false, err
	}
	defer v.Free()
	info, err := v.GetInfo()
	if err != nil {
		return "", false, err
	}
	path, err = v.GetPath()
	return path, info.Type == golibvirt.STORAGE_VOL_BLOCK, err
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
	d, err := c.conn.LookupDomainByName(name)
	if IsNotFound(err) {
		return Domain{}, nil
	} else if err != nil {
		return Domain{}, err
	}
	defer d.Free()
	xml, err := d.GetXMLDesc(golibvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return Domain{}, err
	}
	state, reason, err := d.GetState()
	if err != nil {
		return Domain{}, err
	}
	s, r := stateNames(state, reason)
	return Domain{Found: true, XML: xml, State: s, Reason: r}, nil
}

// DefineDomain 定义实例并关闭 autostart：实例只由 Agent 或 drbd-reactor 启动（ADR-0015）。
func (c *Client) DefineDomain(xml string) error {
	d, err := c.conn.DomainDefineXML(xml)
	if err != nil {
		return err
	}
	defer d.Free()
	return d.SetAutostart(false)
}

func (c *Client) StartDomain(name string) error {
	d, err := c.conn.LookupDomainByName(name)
	if err != nil {
		return err
	}
	defer d.Free()
	return d.Create()
}

// RemoveDomain 强制停止并删除定义（连同 NVRAM），不删除磁盘。
func (c *Client) RemoveDomain(name string) error {
	d, err := c.conn.LookupDomainByName(name)
	if IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	defer d.Free()
	state, _, err := d.GetState()
	if err != nil {
		return err
	}
	if state != golibvirt.DOMAIN_SHUTOFF {
		if err := d.Destroy(); err != nil {
			return err
		}
	}
	err = d.UndefineFlags(golibvirt.DOMAIN_UNDEFINE_NVRAM)
	if isUnsupportedFlags(err) { // test 驱动不支持 NVRAM flag；QEMU 驱动支持
		err = d.Undefine()
	}
	return err
}

// NodeInfo 返回本机的 CPU 数与内存（字节）。
func (c *Client) NodeInfo() (cpus int64, memory int64, err error) {
	info, err := c.conn.GetNodeInfo()
	if err != nil {
		return 0, 0, err
	}
	return int64(info.Cpus), int64(info.Memory) * 1024, nil
}

// IsNotFound 判断 domain、network、pool 或卷不存在。
func IsNotFound(err error) bool {
	var e golibvirt.Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Code {
	case golibvirt.ERR_NO_DOMAIN, golibvirt.ERR_NO_NETWORK, golibvirt.ERR_NO_STORAGE_POOL, golibvirt.ERR_NO_STORAGE_VOL:
		return true
	}
	return false
}

// inactiveXML 读取持久配置。libvirt 的 test 驱动不支持 network 与 pool 的 INACTIVE flag，
// 此时退回不带 flag 读取：test 驱动中两者相同；QEMU 驱动支持该 flag，不会走到这里。
func inactiveXML(get func(inactive bool) (string, error)) (string, error) {
	xml, err := get(true)
	if isUnsupportedFlags(err) {
		return get(false)
	}
	return xml, err
}

func isUnsupportedFlags(err error) bool {
	var e golibvirt.Error
	return errors.As(err, &e) && e.Code == golibvirt.ERR_INVALID_ARG
}

func stateNames(state golibvirt.DomainState, reason int) (string, string) {
	switch state {
	case golibvirt.DOMAIN_RUNNING:
		return "running", pick(runningReasons, reason)
	case golibvirt.DOMAIN_SHUTOFF:
		return "shut off", pick(shutoffReasons, reason)
	case golibvirt.DOMAIN_PAUSED:
		return "paused", "unknown"
	case golibvirt.DOMAIN_SHUTDOWN:
		return "in shutdown", "unknown"
	case golibvirt.DOMAIN_CRASHED:
		return "crashed", "unknown"
	case golibvirt.DOMAIN_PMSUSPENDED:
		return "pmsuspended", "unknown"
	case golibvirt.DOMAIN_BLOCKED:
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

func pick(names []string, i int) string {
	if i < 0 || i >= len(names) {
		return "unknown"
	}
	return names[i]
}
