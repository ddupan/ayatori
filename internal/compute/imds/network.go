package imds

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	// Address 是元数据服务的地址。
	Address = "169.254.169.254"
	// dummyLink 承载 Address：宿主机在实例的 bridge 上不一定有 IP（bridge 直连物理 VLAN），
	// 地址放在独立的 dummy 接口上，回包按策略路由从 bridge 发出。
	dummyLink = "ayimds0"
	// markBase 是 imds 使用的 packet mark 与路由表号的高位；低 16 位是端口的槽位。
	markBase = 0x1a70000
	markMask = 0xffff0000
	// rulePriority 是 imds 的策略路由规则的优先级，按它识别并清理过期的规则。
	rulePriority = 1000
	// listenBase 是实例专属监听端口的起点：第 n 个槽位监听 Address:listenBase+n。
	listenBase = 16900
	table      = "ayatori-imds"
)

// binding 是一个已接入 bridge 的实例端口及其槽位。
type binding struct {
	port   string
	bridge string
	mac    string
	slot   int
}

func (b binding) mark() int { return markBase | b.slot }

// renderRules 生成 nftables 规则，作为一个事务整体替换（ADR-0016、PoC 04）：
//   - arp：丢弃对 Address 的 ARP 请求，宿主机不在任何链路上应答这个地址，避免泄漏到上联口；
//   - bridge：按入口端口打 mark，目的 MAC 改为 bridge 的 MAC 并标记为发给本机：实例的网关通常在
//     外部，只改 MAC 不够，bridge 在 hook 之前已经判定帧不是发给本机的（PoC 07）。回包只能从同一个
//     端口发出：同一链路上的邻居把实例当路由器时，请求从实例的端口进来，回包却会直接发给邻居，
//     TTL=1 拦不住；限制出口后，回包只能交给实例，再转发一跳就被 TTL=1 丢弃；
//   - ip：按 mark DNAT 到该端口专属的监听端口，并把 mark 存进 connmark；本机发出的回包取回 mark，
//     由策略路由从实例所在的 bridge 发出。
//
// 只认入口端口，不看源 IP：客户机可以伪造 IP，但无法伪造自己接在哪个端口上。
func renderRules(bs []binding) string {
	var b strings.Builder
	for _, family := range []string{"arp", "bridge", "ip"} {
		fmt.Fprintf(&b, "destroy table %s %s\n", family, table)
	}
	fmt.Fprintf(&b, `table arp %[1]s {
	chain input {
		type filter hook input priority 0; policy accept;
		arp operation request arp daddr ip %[2]s drop
	}
}
table bridge %[1]s {
	chain prerouting {
		type filter hook prerouting priority -300; policy accept;
`, table, Address)
	for _, x := range bs {
		fmt.Fprintf(&b, "\t\tiifname %q ip daddr %s tcp dport 80 meta mark set %#x ether daddr set %s meta pkttype set host\n",
			x.port, Address, x.mark(), x.mac)
	}
	b.WriteString(`	}
	chain output {
		type filter hook output priority 0; policy accept;
`)
	for _, x := range bs {
		fmt.Fprintf(&b, "\t\tmeta mark %#x oifname != %q drop\n", x.mark(), x.port)
	}
	fmt.Fprintf(&b, `	}
}
table ip %[1]s {
	chain prerouting {
		type nat hook prerouting priority -110; policy accept;
`, table)
	for _, x := range bs {
		fmt.Fprintf(&b, "\t\tmeta mark %#x ip daddr %s tcp dport 80 ct mark set meta mark dnat to %s:%d\n",
			x.mark(), Address, Address, listenBase+x.slot)
	}
	fmt.Fprintf(&b, `	}
	chain output {
		type route hook output priority -150; policy accept;
		ct mark & %#x == %#x meta mark set ct mark
	}
}
`, markMask, markBase)
	return b.String()
}

// applyRules 用 nft 原子地替换规则。
func applyRules(rules string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %w: %s", err, out)
	}
	return nil
}

// setupAddress 在 dummy 接口上配置 Address。
func setupAddress() error {
	link, err := netlink.LinkByName(dummyLink)
	if errors.As(err, new(netlink.LinkNotFoundError)) {
		if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: dummyLink}}); err != nil {
			return fmt.Errorf("add %s: %w", dummyLink, err)
		}
		link, err = netlink.LinkByName(dummyLink)
	}
	if err != nil {
		return err
	}
	if err := netlink.AddrReplace(link, &netlink.Addr{IPNet: &net.IPNet{IP: net.ParseIP(Address), Mask: net.CIDRMask(32, 32)}}); err != nil {
		return err
	}
	return netlink.LinkSetUp(link)
}

// applyRoutes 为每个端口建一条 fwmark 规则和一张只有默认路由的表，回包因此从实例所在的 bridge 发出，
// bridge 上不需要宿主机的 IP。bridge 打开 src_valid_mark，源地址校验同样按 mark 查表，否则请求被
// rp_filter 丢弃。不再使用的规则与表一并删除。
func applyRoutes(bs []binding) error {
	want := map[int]binding{}
	for _, b := range bs {
		want[b.mark()] = b
	}
	rules, err := netlink.RuleList(unix.AF_INET)
	if err != nil {
		return err
	}
	have := map[int]bool{}
	for _, r := range rules {
		if r.Priority != rulePriority || uint32(r.Table)&markMask != markBase { //nolint:gosec // 表号由 imds 分配，不会是负数
			continue
		}
		if _, ok := want[r.Table]; ok && r.Mark == uint32(r.Table) { //nolint:gosec // 同上
			have[r.Table] = true
			continue
		}
		if err := netlink.RuleDel(&r); err != nil {
			return fmt.Errorf("delete rule for table %#x: %w", r.Table, err)
		}
		if err := flushTable(r.Table); err != nil {
			return err
		}
	}
	src := net.ParseIP(Address)
	for mark, b := range want {
		if err := os.WriteFile(filepath.Join("/proc/sys/net/ipv4/conf", b.bridge, "src_valid_mark"), []byte("1"), 0o644); err != nil { //nolint:gosec // sysctl
			return err
		}
		link, err := netlink.LinkByName(b.bridge)
		if err != nil {
			return err
		}
		route := &netlink.Route{Table: mark, LinkIndex: link.Attrs().Index, Scope: netlink.SCOPE_LINK, Src: src,
			Dst: &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}}
		if err := netlink.RouteReplace(route); err != nil {
			return fmt.Errorf("route table %#x: %w", mark, err)
		}
		if have[mark] {
			continue
		}
		rule := netlink.NewRule()
		rule.Family, rule.Priority, rule.Table, rule.Mark = unix.AF_INET, rulePriority, mark, uint32(mark) //nolint:gosec // 同上
		if err := netlink.RuleAdd(rule); err != nil {
			return fmt.Errorf("rule for table %#x: %w", mark, err)
		}
	}
	return nil
}

func flushTable(t int) error {
	routes, err := netlink.RouteListFiltered(unix.AF_INET, &netlink.Route{Table: t}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return err
	}
	for _, r := range routes {
		if err := netlink.RouteDel(&r); err != nil {
			return err
		}
	}
	return nil
}

// portBridge 返回端口当前接入的 bridge 及其 MAC；端口不存在（实例未运行）或未接入 bridge 时返回空。
func portBridge(port string) (bridge, mac string, err error) {
	target, err := os.Readlink(filepath.Join("/sys/class/net", port, "master"))
	if errors.Is(err, os.ErrNotExist) {
		return "", "", nil
	} else if err != nil {
		return "", "", err
	}
	bridge = filepath.Base(target)
	addr, err := os.ReadFile(filepath.Join("/sys/class/net", bridge, "address")) //nolint:gosec // sysfs
	if err != nil {
		return "", "", err
	}
	return bridge, strings.TrimSpace(string(addr)), nil
}
