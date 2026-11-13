package nodeapi

import (
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"time"
)

// Listen 绑定节点控制端口并终止 TLS 1.3 双向认证。
// 默认只允许回环与私网地址：节点持有 root 与 Docker 权限，公网暴露必须由部署显式声明。
func Listen(address string, configuration *tls.Config, allowPublic bool) (net.Listener, error) {
	if configuration == nil || configuration.MinVersion < tls.VersionTLS13 {
		return nil, errors.New("节点监听缺少 TLS 1.3 配置")
	}
	// 端口 0 表示由内核分配，只用于测试；部署配置另有最少端口要求。
	parsed, err := netip.ParseAddrPort(address)
	if err != nil || parsed.Port() > 65535 {
		return nil, errors.New("节点监听地址无效")
	}
	if !allowPublic && !isPrivateAddress(parsed.Addr()) {
		return nil, errors.New("节点监听公网地址需要显式允许")
	}
	listener, err := net.Listen("tcp", parsed.String())
	if err != nil {
		return nil, errors.New("节点监听端口无法绑定")
	}
	return tls.NewListener(listener, configuration), nil
}

// isPrivateAddress 只放行回环、私网、链路本地与 IPv6 唯一本地地址。
func isPrivateAddress(address netip.Addr) bool {
	return address.IsValid() && !address.IsUnspecified() && (address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast())
}

// remaining 把剩余时长换成毫秒；不足一毫秒按一毫秒计，避免边界上变成立即过期。
func remaining(deadline time.Time, maximum time.Duration) int64 {
	value := time.Until(deadline)
	if value <= 0 {
		return 0
	}
	if value > maximum {
		value = maximum
	}
	millis := (value + time.Millisecond - 1) / time.Millisecond
	if millis < 1 {
		return 1
	}
	return int64(millis)
}
