// Package networkpolicy 定义模型与用户电脑出网共用的地址分类规则。
package networkpolicy

import "net/netip"

// Prohibited 拒绝非单播、本机、链路本地、转换隧道及固定云节点地址，不能被私网许可覆盖。
func Prohibited(address netip.Addr) bool {
	if !address.IsValid() || address.Zone() != "" || !address.IsGlobalUnicast() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return true
	}
	// 转换或隧道地址可能在网络层转向另一个 IPv4 目标，不能用 IPv6 放行规则绕过地址检查。
	for _, prefix := range []string{"64:ff9b::/96", "64:ff9b:1::/48", "2002::/16", "2001::/32"} {
		if netip.MustParsePrefix(prefix).Contains(address) {
			return true
		}
	}
	// 保留的云节点地址不能通过宽泛的内网模型允许范围放行。
	return address == netip.MustParseAddr("fd00:ec2::254") || address == netip.MustParseAddr("168.63.129.16")
}

// IsPublic 仅放行可路由公网地址；调用前需 Unmap，部署仍须额外排除宿主及控制网络。
func IsPublic(address netip.Addr) bool {
	if Prohibited(address) || address.IsPrivate() {
		return false
	}
	for _, reserved := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001::/23", "2002::/16"} {
		if netip.MustParsePrefix(reserved).Contains(address) {
			return false
		}
	}
	return address.Is4() || netip.MustParsePrefix("2000::/3").Contains(address)
}
