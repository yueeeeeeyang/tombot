// Package egressproxy 为用户电脑提供受限 HTTP 与 CONNECT 出网，不持有模型或数据库凭据。
package egressproxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"io.yueyang/tom/internal/networkpolicy"
)

var (
	ErrAddressDenied = errors.New("电脑出网目标不允许")
	ErrUnavailable   = errors.New("电脑出网连接失败，操作结果可能未知")
	ErrLimit         = errors.New("电脑出网传输超过限额")
)

// Options 只由受信节点部署提供；用户代码不能改变地址策略、时限或连接配额。
type Options struct {
	Ports                                                                                      []uint16
	DeniedNetworks                                                                             []netip.Prefix
	ConnectTimeout, HeaderTimeout, IdleTimeout, RequestTimeout, TunnelTimeout, ShutdownTimeout time.Duration
	MaximumConnections                                                                         int
	MaximumRequestBytes, MaximumResponseBytes, MaximumTunnelBytes                              int64
}

func (options Options) Validate() error {
	if len(options.Ports) < 1 || len(options.Ports) > 64 || len(options.DeniedNetworks) > 256 || options.MaximumConnections < 1 || options.MaximumConnections > 256 {
		return errors.New("电脑出网策略或连接上限无效")
	}
	seen := map[uint16]bool{}
	for _, port := range options.Ports {
		if port == 0 || seen[port] {
			return errors.New("电脑出网端口规则无效")
		}
		seen[port] = true
	}
	for _, prefix := range options.DeniedNetworks {
		if !prefix.IsValid() || prefix != prefix.Masked() || prefix.Bits() == 0 || prefix.Addr().Is4In6() || prefix.Addr().Zone() != "" {
			return errors.New("电脑出网拒绝网段无效")
		}
	}
	for _, duration := range []time.Duration{options.ConnectTimeout, options.HeaderTimeout, options.IdleTimeout, options.ShutdownTimeout} {
		if duration < time.Second || duration > time.Minute {
			return errors.New("电脑出网短调用时限无效")
		}
	}
	for _, duration := range []time.Duration{options.RequestTimeout, options.TunnelTimeout} {
		if duration < time.Second || duration > time.Hour {
			return errors.New("电脑出网传输时限无效")
		}
	}
	if options.ConnectTimeout > options.RequestTimeout || options.HeaderTimeout > options.RequestTimeout || options.ConnectTimeout > options.TunnelTimeout || options.ShutdownTimeout < options.ConnectTimeout {
		return errors.New("电脑出网时限顺序无效")
	}
	for _, size := range []int64{options.MaximumRequestBytes, options.MaximumResponseBytes, options.MaximumTunnelBytes} {
		if size < 1024 || size > 1<<40 {
			return errors.New("电脑出网传输大小上限无效")
		}
	}
	return nil
}

type destination struct{ host, port string }

func target(authority string, defaultPort string) (destination, error) {
	if len(authority) == 0 || len(authority) > 512 || strings.ContainsAny(authority, "@\\/%?#\t\r\n ") {
		return destination{}, ErrAddressDenied
	}
	parsed, err := url.Parse("http://" + authority)
	if err != nil || parsed.Hostname() == "" || parsed.Host != authority || parsed.User != nil || strings.HasSuffix(authority, ":") {
		return destination{}, ErrAddressDenied
	}
	host := strings.ToLower(parsed.Hostname())
	if literal, err := netip.ParseAddr(host); err == nil {
		if literal.Zone() != "" {
			return destination{}, ErrAddressDenied
		}
		host = literal.Unmap().String()
	} else {
		if len(host) > 253 || strings.HasSuffix(host, ".") {
			return destination{}, ErrAddressDenied
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return destination{}, ErrAddressDenied
			}
			for _, character := range label {
				if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-') {
					return destination{}, ErrAddressDenied
				}
			}
		}
	}
	port := parsed.Port()
	if port == "" {
		port = defaultPort
	}
	value, err := strconv.ParseUint(port, 10, 16)
	if err != nil || value == 0 || strconv.FormatUint(value, 10) != port {
		return destination{}, ErrAddressDenied
	}
	return destination{host, port}, nil
}

type resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}
type dialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

func localAddresses() ([]netip.Addr, error) {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil, ErrUnavailable
	}
	result := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		prefix, err := netip.ParsePrefix(address.String())
		if err != nil {
			return nil, ErrUnavailable
		}
		result = append(result, prefix.Addr().Unmap())
	}
	return result, nil
}

func (proxy *Server) connect(parent context.Context, target destination) (net.Conn, error) {
	allowedPort := false
	for _, port := range proxy.options.Ports {
		if target.port == strconv.Itoa(int(port)) {
			allowedPort = true
			break
		}
	}
	if !allowedPort {
		return nil, ErrAddressDenied
	}
	ctx, cancel := context.WithTimeout(parent, proxy.options.ConnectTimeout)
	defer cancel()
	addresses := []netip.Addr{}
	if literal, err := netip.ParseAddr(target.host); err == nil {
		addresses = append(addresses, literal)
	} else {
		resolved, err := proxy.resolver.LookupNetIP(ctx, "ip", target.host)
		if err != nil {
			return nil, ErrUnavailable
		}
		addresses = resolved
	}
	if len(addresses) == 0 || len(addresses) > 16 {
		return nil, ErrAddressDenied
	}
	local, err := proxy.localAddresses()
	if err != nil {
		return nil, ErrUnavailable
	}
	for _, address := range addresses {
		address = address.Unmap()
		if !networkpolicy.IsPublic(address) {
			return nil, ErrAddressDenied
		}
		for _, prefix := range proxy.options.DeniedNetworks {
			if prefix.Contains(address) {
				return nil, ErrAddressDenied
			}
		}
		for _, self := range local {
			if address == self.Unmap() {
				return nil, ErrAddressDenied
			}
		}
	}
	// 只建立一次数字地址连接；禁止再次解析或在发送结果未知时切换地址重发。
	connection, err := proxy.dialer.DialContext(ctx, "tcp", net.JoinHostPort(addresses[0].Unmap().String(), target.port))
	if err != nil {
		return nil, ErrUnavailable
	}
	deadline, _ := parent.Deadline()
	return &timedConnection{Conn: connection, idle: proxy.options.IdleTimeout, deadline: deadline}, nil
}

type timedConnection struct {
	net.Conn
	idle     time.Duration
	deadline time.Time
}

func (connection *timedConnection) nextDeadline() time.Time {
	next := time.Now().Add(connection.idle)
	if !connection.deadline.IsZero() && connection.deadline.Before(next) {
		return connection.deadline
	}
	return next
}
func (connection *timedConnection) Read(buffer []byte) (int, error) {
	if err := connection.Conn.SetReadDeadline(connection.nextDeadline()); err != nil {
		return 0, err
	}
	return connection.Conn.Read(buffer)
}
func (connection *timedConnection) Write(buffer []byte) (int, error) {
	if err := connection.Conn.SetWriteDeadline(connection.nextDeadline()); err != nil {
		return 0, err
	}
	return connection.Conn.Write(buffer)
}
