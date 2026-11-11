// Package naming 是 Compute Agent 与 ayatori-imds 共同遵守的宿主机侧命名（ADR-0016）。
package naming

import (
	"fmt"
	"strings"
)

// PortName 是实例第 i 块网卡在宿主机上的端口名。固定下来，按端口写的 nftables 规则（元数据服务）
// 在冷启动后仍然有效；Linux 接口名最长 15 字符（ADR-0016）。
// ponytail: 取 UID 前 8 个十六进制字符，同一节点上的实例碰撞概率可忽略；规模变大时改用节点内分配。
func PortName(uid string, i int) string {
	return fmt.Sprintf("ay%sn%d", strings.ReplaceAll(uid, "-", "")[:8], i)
}
