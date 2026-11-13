// Package libvirt 是 Compute Agent 访问本机 libvirt 的薄 adapter：只做调用与翻译，返回事实，
// 不含"何时下发、何时启动"的策略。使用 libvirt 官方的 cgo 绑定。
package libvirt

import (
	"errors"
	"fmt"
	"io"
	"sync"

	golibvirt "libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
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
	defer free(n.Free)
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
	defer free(n.Free)
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
	defer free(n.Free)
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
	defer free(p.Free)
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
	defer free(p.Free)
	if err := p.SetAutostart(true); err != nil {
		return err
	}
	if active, err := p.IsActive(); err != nil || active {
		return err
	}
	return p.Create(0)
}

// PoolSpace 返回 pool 的容量与可用空间（字节）。
func (c *Client) PoolSpace(name string) (capacity, available uint64, err error) {
	p, err := c.conn.LookupStoragePoolByName(name)
	if err != nil {
		return 0, 0, err
	}
	defer free(p.Free)
	info, err := p.GetInfo()
	if err != nil {
		return 0, 0, err
	}
	return info.Capacity, info.Available, nil
}

// PoolDir 返回 dir 类型 pool 的目录；其他类型报错。
func (c *Client) PoolDir(name string) (string, error) {
	p, err := c.conn.LookupStoragePoolByName(name)
	if err != nil {
		return "", err
	}
	defer free(p.Free)
	desc, err := p.GetXMLDesc(0)
	if err != nil {
		return "", err
	}
	var x libvirtxml.StoragePool
	if err := x.Unmarshal(desc); err != nil {
		return "", err
	}
	if x.Type != "dir" || x.Target == nil {
		return "", fmt.Errorf("pool %s has type %s, need dir", name, x.Type)
	}
	return x.Target.Path, nil
}

// RefreshPool 让 libvirt 重新读取 pool 中的文件。
func (c *Client) RefreshPool(name string) error {
	p, err := c.conn.LookupStoragePoolByName(name)
	if err != nil {
		return err
	}
	defer free(p.Free)
	return p.Refresh(0)
}

// RemovePool 停止并删除定义，不删除底层存储。
func (c *Client) RemovePool(name string) error {
	p, err := c.conn.LookupStoragePoolByName(name)
	if IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	defer free(p.Free)
	if active, err := p.IsActive(); err != nil {
		return err
	} else if active {
		if err := p.Destroy(); err != nil {
			return err
		}
	}
	return p.Undefine()
}

// Volume 是卷在本机的现状。
type Volume struct {
	Found    bool
	Path     string
	Block    bool
	Capacity uint64
	// Format 是卷 XML 中的格式，例如 qcow2、raw；pool 不报告格式（例如 logical）时为空。
	Format string
}

// Volume 返回 pool 中名为 name 的卷。libvirt 只在 refresh 时发现 pool 目录里新出现的文件，因此
// 找不到时先 refresh 一次再查：带外放进去的文件（例如迁入的磁盘）也能被引用和接管。
func (c *Client) Volume(pool, name string) (Volume, error) {
	p, err := c.conn.LookupStoragePoolByName(pool)
	if err != nil {
		return Volume{}, err
	}
	defer free(p.Free)
	v, err := p.LookupStorageVolByName(name)
	if IsNotFound(err) {
		if err := p.Refresh(0); err != nil {
			return Volume{}, err
		}
		v, err = p.LookupStorageVolByName(name)
	}
	if IsNotFound(err) {
		return Volume{}, nil
	} else if err != nil {
		return Volume{}, err
	}
	defer free(v.Free)
	info, err := v.GetInfo()
	if err != nil {
		return Volume{}, err
	}
	path, err := v.GetPath()
	if err != nil {
		return Volume{}, err
	}
	desc, err := v.GetXMLDesc(0)
	if err != nil {
		return Volume{}, err
	}
	var x libvirtxml.StorageVolume
	if err := x.Unmarshal(desc); err != nil {
		return Volume{}, err
	}
	vol := Volume{Found: true, Path: path, Block: info.Type == golibvirt.STORAGE_VOL_BLOCK, Capacity: info.Capacity}
	if x.Target != nil && x.Target.Format != nil {
		vol.Format = x.Target.Format.Type
	}
	return vol, nil
}

// CreateVolume 按原生 volume XML 在 pool 中创建卷。
func (c *Client) CreateVolume(pool, xml string) error {
	p, err := c.conn.LookupStoragePoolByName(pool)
	if err != nil {
		return err
	}
	defer free(p.Free)
	v, err := p.StorageVolCreateXML(xml, 0)
	if err != nil {
		return err
	}
	return v.Free()
}

// CreateVolumeFrom 按 volume XML 在 pool 中创建卷，内容完整复制自 sourcePool 中的 sourceName。
// libvirt 复制时忽略比来源大的 capacity，这里再扩容到 capacity（字节）；比来源小时保持来源大小。
// 扩容失败就删掉新卷：卷已存在时不再修改，留下来就永远是错的大小。
func (c *Client) CreateVolumeFrom(pool, xml, sourcePool, sourceName string, capacity uint64) error {
	p, err := c.conn.LookupStoragePoolByName(pool)
	if err != nil {
		return err
	}
	defer free(p.Free)
	sp, err := c.conn.LookupStoragePoolByName(sourcePool)
	if err != nil {
		return err
	}
	defer free(sp.Free)
	src, err := sp.LookupStorageVolByName(sourceName)
	if err != nil {
		return err
	}
	defer free(src.Free)
	v, err := p.StorageVolCreateXMLFrom(xml, src, 0)
	if err != nil {
		return err
	}
	defer free(v.Free)
	info, err := v.GetInfo()
	if err == nil && info.Capacity < capacity {
		err = v.Resize(capacity, 0)
	}
	if err != nil {
		return errors.Join(err, v.Delete(0))
	}
	return nil
}

// ResizeVolume 把卷扩大到 capacity（字节）。
func (c *Client) ResizeVolume(pool, name string, capacity uint64) error {
	p, err := c.conn.LookupStoragePoolByName(pool)
	if err != nil {
		return err
	}
	defer free(p.Free)
	v, err := p.LookupStorageVolByName(name)
	if err != nil {
		return err
	}
	defer free(v.Free)
	return v.Resize(capacity, 0)
}

// DeleteVolume 删除卷及其数据；卷不存在时什么也不做，pool 不存在时报错。
func (c *Client) DeleteVolume(pool, name string) error {
	p, err := c.conn.LookupStoragePoolByName(pool)
	if err != nil {
		// pool 不存在时无从判断卷是否还在，不能当作已删除：卷文件可能仍在磁盘上。
		return err
	}
	defer free(p.Free)
	v, err := p.LookupStorageVolByName(name)
	if IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	defer free(v.Free)
	return v.Delete(0)
}

// Domain 是实例的现状；State 与 Reason 与 `virsh domstate --reason` 的写法一致。
type Domain struct {
	Found  bool
	UUID   string
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
	defer free(d.Free)
	xml, err := d.GetXMLDesc(golibvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return Domain{}, err
	}
	state, reason, err := d.GetState()
	if err != nil {
		return Domain{}, err
	}
	uuid, err := d.GetUUIDString()
	if err != nil {
		return Domain{}, err
	}
	s, r := stateNames(state, reason)
	return Domain{Found: true, UUID: uuid, XML: xml, State: s, Reason: r}, nil
}

// LiveDomain 返回运行中实例的 live 配置：libvirt 填写的设备 alias、控制台 socket 路径等只在其中。
func (c *Client) LiveDomain(name string) (*libvirtxml.Domain, error) {
	d, err := c.conn.LookupDomainByName(name)
	if err != nil {
		return nil, err
	}
	defer free(d.Free)
	xml, err := d.GetXMLDesc(0)
	if err != nil {
		return nil, err
	}
	var dom libvirtxml.Domain
	if err := dom.Unmarshal(xml); err != nil {
		return nil, err
	}
	return &dom, nil
}

// Console 是实例字符设备的字节流（阻塞模式）。读写可以在两个 goroutine 中并发进行，Close 可以与它们并发。
type Console struct {
	s *golibvirt.Stream
	// mu：读写持读锁；Close 先中止流让阻塞的读写返回，再持写锁释放流，不会释放仍在使用的流。
	mu     sync.RWMutex
	closed bool
}

func (c *Console) Read(p []byte) (int, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return 0, io.EOF
	}
	return c.s.Recv(p)
}

func (c *Console) Write(p []byte) (int, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return 0, io.ErrClosedPipe
	}
	return c.s.Send(p)
}

// Close 中止并释放流，可以重复调用。
func (c *Console) Close() error {
	c.mu.RLock()
	closed := c.closed
	c.mu.RUnlock()
	if closed {
		return nil
	}
	err := c.s.Abort()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	free(c.s.Free)
	return err
}

// OpenConsole 打开实例上 alias 对应的串口或 console。已有会话（包括 virsh console）时由新会话接管，
// 旧会话的流随之结束：避免残留的会话把控制台永久占住。
func (c *Client) OpenConsole(name, alias string) (*Console, error) {
	d, err := c.conn.LookupDomainByName(name)
	if err != nil {
		return nil, err
	}
	defer free(d.Free)
	s, err := c.conn.NewStream(0)
	if err != nil {
		return nil, err
	}
	if err := d.OpenConsole(alias, s, golibvirt.DOMAIN_CONSOLE_FORCE); err != nil {
		free(s.Free)
		return nil, err
	}
	return &Console{s: s}, nil
}

// DefineDomain 定义实例并关闭 autostart：实例只由 Agent 或 drbd-reactor 启动（ADR-0015）。
func (c *Client) DefineDomain(xml string) error {
	d, err := c.conn.DomainDefineXML(xml)
	if err != nil {
		return err
	}
	defer free(d.Free)
	return d.SetAutostart(false)
}

// BlockCapacity 返回运行中的实例看到的磁盘容量（字节），path 是磁盘的源路径。
func (c *Client) BlockCapacity(domain, path string) (uint64, error) {
	d, err := c.conn.LookupDomainByName(domain)
	if err != nil {
		return 0, err
	}
	defer free(d.Free)
	info, err := d.GetBlockInfo(path, 0)
	if err != nil {
		return 0, err
	}
	return info.Capacity, nil
}

// BlockResize 在线把实例的磁盘扩大到 capacity（字节）。
func (c *Client) BlockResize(domain, path string, capacity uint64) error {
	d, err := c.conn.LookupDomainByName(domain)
	if err != nil {
		return err
	}
	defer free(d.Free)
	return d.BlockResize(path, capacity, golibvirt.DOMAIN_BLOCK_RESIZE_BYTES)
}

func (c *Client) StartDomain(name string) error {
	d, err := c.conn.LookupDomainByName(name)
	if err != nil {
		return err
	}
	defer free(d.Free)
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
	defer free(d.Free)
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

// HostCPU 返回 capabilities 中本机 CPU 的厂商、型号与 CPUID 签名。
func (c *Client) HostCPU() (libvirtxml.CapsHostCPU, error) {
	doc, err := c.conn.GetCapabilities()
	if err != nil {
		return libvirtxml.CapsHostCPU{}, err
	}
	var caps libvirtxml.Caps
	if err := caps.Unmarshal(doc); err != nil {
		return libvirtxml.CapsHostCPU{}, err
	}
	if caps.Host.CPU == nil {
		return libvirtxml.CapsHostCPU{}, nil
	}
	return *caps.Host.CPU, nil
}

// CPUProvides 报告本机的 hypervisor 能否提供 xml 描述的 CPU。
func (c *Client) CPUProvides(xml string) (bool, error) {
	result, err := c.conn.CompareHypervisorCPU("", "", "", "", xml, 0)
	if err != nil {
		return false, err
	}
	return result != golibvirt.CPU_COMPARE_INCOMPATIBLE, nil
}

// DomainStats 是运行中实例的资源用量。累计量自实例启动起计。
type DomainStats struct {
	Name string
	// CPUTime 是累计 CPU 时间（纳秒）。
	CPUTime uint64
	// Memory 是 balloon 报告的内存（字节）：current 为客户机当前可用，maximum 为配置量，rss 为宿主机
	// 上 QEMU 进程的驻留内存。客户机不支持 balloon 时缺项。
	Memory map[string]uint64
	Disks  []IOStats
	Nics   []IOStats
}

// IOStats 是一块磁盘或一块网卡的累计字节数：磁盘为读、写，网卡为收、发。
type IOStats struct {
	Device  string
	In, Out uint64
}

// AllDomainStats 返回所有运行中实例的资源用量。
func (c *Client) AllDomainStats() ([]DomainStats, error) {
	records, err := c.conn.GetAllDomainStats(nil,
		golibvirt.DOMAIN_STATS_CPU_TOTAL|golibvirt.DOMAIN_STATS_BALLOON|golibvirt.DOMAIN_STATS_BLOCK|golibvirt.DOMAIN_STATS_INTERFACE,
		golibvirt.CONNECT_GET_ALL_DOMAINS_STATS_ACTIVE)
	if err != nil {
		return nil, err
	}
	stats := make([]DomainStats, 0, len(records))
	for _, r := range records {
		name, err := r.Domain.GetName()
		free(r.Domain.Free)
		if err != nil {
			return nil, err
		}
		s := DomainStats{Name: name, Memory: map[string]uint64{}}
		if r.Cpu != nil && r.Cpu.TimeSet {
			s.CPUTime = r.Cpu.Time
		}
		if b := r.Balloon; b != nil {
			if b.CurrentSet {
				s.Memory["current"] = b.Current * 1024
			}
			if b.MaximumSet {
				s.Memory["maximum"] = b.Maximum * 1024
			}
			if b.RssSet {
				s.Memory["rss"] = b.Rss * 1024
			}
		}
		for _, b := range r.Block {
			s.Disks = append(s.Disks, IOStats{Device: b.Name, In: b.RdBytes, Out: b.WrBytes})
		}
		for _, n := range r.Net {
			s.Nics = append(s.Nics, IOStats{Device: n.Name, In: n.RxBytes, Out: n.TxBytes})
		}
		stats = append(stats, s)
	}
	return stats, nil
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

const (
	unknown = "unknown"
	crashed = "crashed"
)

func stateNames(state golibvirt.DomainState, reason int) (string, string) {
	switch state {
	case golibvirt.DOMAIN_RUNNING:
		return "running", pick(runningReasons, reason)
	case golibvirt.DOMAIN_SHUTOFF:
		return "shut off", pick(shutoffReasons, reason)
	case golibvirt.DOMAIN_PAUSED:
		return "paused", unknown
	case golibvirt.DOMAIN_SHUTDOWN:
		return "in shutdown", unknown
	case golibvirt.DOMAIN_CRASHED:
		return crashed, unknown
	case golibvirt.DOMAIN_PMSUSPENDED:
		return "pmsuspended", unknown
	case golibvirt.DOMAIN_BLOCKED:
		return "idle", unknown
	}
	return "no state", unknown
}

var (
	runningReasons = []string{unknown, "booted", "migrated", "restored", "from snapshot", "unpaused",
		"migration canceled", "save canceled", "event wakeup", crashed, "post-copy", "post-copy failed"}
	shutoffReasons = []string{unknown, "shutdown", "destroyed", crashed, "migrated", "saved", "failed",
		"from snapshot", "daemon"}
)

// free 释放 libvirt 对象的引用；只在返回前调用，失败也无事可做。
func free(f func() error) { _ = f() }

func pick(names []string, i int) string {
	if i < 0 || i >= len(names) {
		return unknown
	}
	return names[i]
}
