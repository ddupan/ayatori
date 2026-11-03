// Package powerdns 经 PowerDNS Authoritative HTTP API 维护 LoadBalancer 的 A 记录，只返回事实。
//
// 部署契约：zone 由 homelab-infra 创建并委派，记录只由 Ayatori 写入。
package powerdns

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"slices"

	pdns "github.com/joeig/go-powerdns/v3"
)

// TTL 较短，使同名重建后新 VIP 尽快生效；VIP 本身一经分配不变。
const recordTTL = 60

// Zone 是 Ayatori 独占的 zone。
type Zone struct {
	Client *pdns.Client
	Name   string
}

// New 以 API key 访问 PowerDNS 的 localhost server。
func New(url, apiKey, zone string, httpClient *http.Client) *Zone {
	return &Zone{Client: pdns.New(url, "localhost", pdns.WithAPIKey(apiKey), pdns.WithHTTPClient(httpClient)), Name: zone}
}

// Record 返回名称上的 A 记录值，按地址排序；不存在时为空。
func (z *Zone) Record(ctx context.Context, name string) ([]netip.Addr, error) {
	rrsets, err := z.Client.Records.Get(ctx, z.Name, canonical(name), new(pdns.RRTypeA))
	if err != nil {
		return nil, err
	}
	var addresses []netip.Addr
	for _, rrset := range rrsets {
		for _, record := range rrset.Records {
			if record.Content == nil || (record.Disabled != nil && *record.Disabled) {
				continue
			}
			addr, err := netip.ParseAddr(*record.Content)
			if err != nil {
				return nil, errors.Join(errors.New("unexpected A record content "+*record.Content), err)
			}
			addresses = append(addresses, addr)
		}
	}
	slices.SortFunc(addresses, netip.Addr.Compare)
	return addresses, nil
}

// Publish 以唯一值替换名称上的 A 记录。
func (z *Zone) Publish(ctx context.Context, name string, vip netip.Addr) error {
	return z.Client.Records.Change(ctx, z.Name, canonical(name), pdns.RRTypeA, recordTTL, []string{vip.String()})
}

// Remove 删除名称上的 A 记录；不存在时同样成功。
func (z *Zone) Remove(ctx context.Context, name string) error {
	return z.Client.Records.Delete(ctx, z.Name, canonical(name), pdns.RRTypeA)
}

func canonical(name string) string { return name + "." }
