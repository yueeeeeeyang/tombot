// Package modelgateway 执行受管模型请求与能力探测，不持有数据库或长期供应商凭据。
package modelgateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"io.yueyang/tom/internal/networkpolicy"
)

var (
	ErrInvalidInput     = errors.New("模型调用参数无效")
	ErrAddressDenied    = errors.New("模型地址不在部署允许范围内")
	ErrUnavailable      = errors.New("模型请求失败，结果可能未知")
	ErrRequestTooLarge  = errors.New("模型请求超过大小上限")
	ErrResponseTooLarge = errors.New("模型响应超过大小上限")
	ErrProtocol         = errors.New("模型响应协议无效")
	ErrVerification     = errors.New("模型未通过声明能力验证")
)

// Origin 由部署配置允许的精确 HTTPS 来源。Networks 非空时将解析地址限制在指定网段内。
// 空 Networks 只允许公网地址；私有模型必须显式指定范围，禁止环回、链路本地及部署拒绝网段。
type Origin struct {
	URL      string
	Networks []netip.Prefix
}

// Options 的网络边界、证书和时限只能由部署注入，管理模型接口不能修改这些限制。
type Options struct {
	Origins               []Origin
	DeniedNetworks        []netip.Prefix
	RootCAs               *x509.CertPool
	ConnectTimeout        time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	IdleReadTimeout       time.Duration
	WriteTimeout          time.Duration
	RequestTimeout        time.Duration
	MaxRequestBytes       int64
	MaxResponseBytes      int64
	MaxConcurrentRequests int
}

type rule struct {
	host, port string
	networks   []netip.Prefix
}
type resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}
type dialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

// Gateway 共享有界调用槽位；每个调用建立独立连接，避免跨来源复用和过期 DNS 决策。
type Gateway struct {
	options  Options
	rules    map[string]rule
	resolver resolver
	dialer   dialer
	slots    chan struct{}
}

// New 校验并复制部署策略；不读取环境代理、不允许关闭 TLS 验证或放宽到所有来源。
func New(options Options) (*Gateway, error) {
	if len(options.Origins) < 1 || len(options.Origins) > 128 || len(options.DeniedNetworks) > 256 || options.MaxConcurrentRequests < 1 || options.MaxConcurrentRequests > 128 ||
		options.MaxRequestBytes < 1024 || options.MaxRequestBytes > 64<<20 || options.MaxResponseBytes < 1024 || options.MaxResponseBytes > 64<<20 {
		return nil, ErrInvalidInput
	}
	for _, duration := range []time.Duration{options.ConnectTimeout, options.TLSHandshakeTimeout, options.ResponseHeaderTimeout, options.IdleReadTimeout, options.WriteTimeout, options.RequestTimeout} {
		if duration < time.Millisecond || duration > 3*time.Minute {
			return nil, ErrInvalidInput
		}
	}
	for _, duration := range []time.Duration{options.ConnectTimeout, options.TLSHandshakeTimeout, options.ResponseHeaderTimeout, options.IdleReadTimeout, options.WriteTimeout} {
		if duration > options.RequestTimeout {
			return nil, ErrInvalidInput
		}
	}
	rules := make(map[string]rule, len(options.Origins))
	for _, origin := range options.Origins {
		parsed, err := parseEndpoint(origin.URL)
		if err != nil || parsed.Path != "" && parsed.Path != "/" || len(origin.Networks) > 64 {
			return nil, ErrInvalidInput
		}
		key := originKey(parsed)
		if _, exists := rules[key]; exists {
			return nil, ErrInvalidInput
		}
		for _, prefix := range origin.Networks {
			if !validPrefix(prefix) {
				return nil, ErrInvalidInput
			}
		}
		rules[key] = rule{host: strings.ToLower(parsed.Hostname()), port: port(parsed), networks: append([]netip.Prefix(nil), origin.Networks...)}
	}
	for _, prefix := range options.DeniedNetworks {
		if !validPrefix(prefix) {
			return nil, ErrInvalidInput
		}
	}
	options.DeniedNetworks = append([]netip.Prefix(nil), options.DeniedNetworks...)
	options.Origins = nil
	if options.RootCAs != nil {
		options.RootCAs = options.RootCAs.Clone()
	}
	return &Gateway{options: options, rules: rules, resolver: net.DefaultResolver, dialer: &net.Dialer{Timeout: options.ConnectTimeout, KeepAlive: -1}, slots: make(chan struct{}, options.MaxConcurrentRequests)}, nil
}

func validPrefix(prefix netip.Prefix) bool {
	return prefix.IsValid() && prefix.Bits() > 0 && prefix == prefix.Masked() && !prefix.Addr().Is4In6() && prefix.Addr().Zone() == ""
}
func port(address *url.URL) string {
	if address.Port() == "" {
		return "443"
	}
	return address.Port()
}
func originKey(address *url.URL) string {
	return "https://" + net.JoinHostPort(strings.ToLower(address.Hostname()), port(address))
}
func parseEndpoint(value string) (*url.URL, error) {
	if len(value) > 2048 || strings.ContainsAny(value, "\\\r\n\t?#") || strings.TrimSpace(value) != value {
		return nil, ErrAddressDenied
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(parsed.Hostname(), "%") || strings.HasSuffix(parsed.Host, ":") {
		return nil, ErrAddressDenied
	}
	for _, character := range parsed.Hostname() {
		if character > 127 {
			return nil, ErrAddressDenied
		}
	}
	valuePort, err := strconv.Atoi(port(parsed))
	if err != nil || valuePort < 1 || valuePort > 65535 {
		return nil, ErrAddressDenied
	}
	return parsed, nil
}

func contains(prefixes []netip.Prefix, address netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func (gateway *Gateway) allowed(target rule, address netip.Addr) bool {
	address = address.Unmap()
	if networkpolicy.Prohibited(address) || contains(gateway.options.DeniedNetworks, address) {
		return false
	}
	if len(target.networks) > 0 {
		return contains(target.networks, address)
	}
	return networkpolicy.IsPublic(address)
}

func (gateway *Gateway) connect(target rule) func(context.Context, string, string) (net.Conn, error) {
	return func(parent context.Context, network, address string) (net.Conn, error) {
		host, destinationPort, err := net.SplitHostPort(address)
		if err != nil || network != "tcp" || !strings.EqualFold(host, target.host) || destinationPort != target.port {
			return nil, ErrAddressDenied
		}
		ctx, cancel := context.WithTimeout(parent, gateway.options.ConnectTimeout)
		defer cancel()
		var addresses []netip.Addr
		if literal, err := netip.ParseAddr(target.host); err == nil {
			addresses = []netip.Addr{literal}
		} else {
			var err error
			addresses, err = gateway.resolver.LookupNetIP(ctx, "ip", target.host)
			if err != nil {
				return nil, ErrUnavailable
			}
		}
		if len(addresses) == 0 || len(addresses) > 16 {
			return nil, ErrAddressDenied
		}
		for _, resolved := range addresses {
			if !gateway.allowed(target, resolved) {
				return nil, ErrAddressDenied
			}
		}
		// 验证解析所得地址后直接连接数字地址，禁止在连接器里再次解析域名。
		for _, resolved := range addresses {
			connection, err := gateway.dialer.DialContext(ctx, "tcp", net.JoinHostPort(resolved.Unmap().String(), target.port))
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue
			}
			return &deadlineConnection{Conn: connection, readTimeout: gateway.options.IdleReadTimeout, writeTimeout: gateway.options.WriteTimeout}, nil
		}
		return nil, ErrUnavailable
	}
}

type deadlineConnection struct {
	net.Conn
	readTimeout, writeTimeout time.Duration
}

func (connection *deadlineConnection) Read(buffer []byte) (int, error) {
	if err := connection.Conn.SetReadDeadline(time.Now().Add(connection.readTimeout)); err != nil {
		return 0, err
	}
	return connection.Conn.Read(buffer)
}
func (connection *deadlineConnection) Write(buffer []byte) (int, error) {
	if err := connection.Conn.SetWriteDeadline(time.Now().Add(connection.writeTimeout)); err != nil {
		return 0, err
	}
	return connection.Conn.Write(buffer)
}

// guardedTransport 固定允许的单个来源与请求方法；发现调用只放行 GET，流式调用只放行 POST。
type guardedTransport struct {
	transport                         *http.Transport
	endpoint                          string
	maxRequestBytes, maxResponseBytes int64
	failure                           error
	method                            string
	contentType                       string
}

func (transport *guardedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != transport.method || request.URL.String() != transport.endpoint || (request.Host != "" && request.Host != request.URL.Host) {
		transport.failure = ErrAddressDenied
		return nil, transport.failure
	}
	if request.ContentLength < 0 || request.ContentLength > transport.maxRequestBytes {
		transport.failure = ErrRequestTooLarge
		return nil, transport.failure
	}
	request.GetBody = nil
	response, err := transport.transport.RoundTrip(request)
	if err != nil {
		if errors.Is(err, ErrAddressDenied) {
			transport.failure = ErrAddressDenied
		}
		return nil, err
	}
	if response.StatusCode == 200 {
		mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if err != nil || mediaType != transport.contentType || response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity" {
			transport.failure = ErrProtocol
			return nil, errors.Join(transport.failure, response.Body.Close())
		}
	}
	response.Body = &limitedBody{ReadCloser: response.Body, remaining: transport.maxResponseBytes, transport: transport}
	return response, nil
}

type limitedBody struct {
	io.ReadCloser
	remaining int64
	transport *guardedTransport
}

func (body *limitedBody) Read(buffer []byte) (int, error) {
	if body.remaining < 0 {
		return 0, ErrResponseTooLarge
	}
	if int64(len(buffer)) > body.remaining+1 {
		buffer = buffer[:body.remaining+1]
	}
	n, err := body.ReadCloser.Read(buffer)
	body.remaining -= int64(n)
	if body.remaining < 0 {
		body.transport.failure = ErrResponseTooLarge
		return 0, ErrResponseTooLarge
	}
	return n, err
}

// client 构造受出口策略约束的 HTTP 客户端；方法与成功响应类型由调用方固定，不接受请求内覆盖。
func (gateway *Gateway) client(endpoint, method, contentType string) (*http.Client, *guardedTransport, error) {
	parsed, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, nil, err
	}
	target, exists := gateway.rules[originKey(parsed)]
	if !exists {
		return nil, nil, ErrAddressDenied
	}
	// HTTP/1.1 独立连接使读写时限不影响其他请求，且每次请求都重新验证 DNS 和策略。
	transport := &http.Transport{Proxy: nil, DialContext: gateway.connect(target), TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: gateway.options.RootCAs, NextProtos: []string{"http/1.1"}},
		TLSHandshakeTimeout: gateway.options.TLSHandshakeTimeout, ResponseHeaderTimeout: gateway.options.ResponseHeaderTimeout, DisableKeepAlives: true, DisableCompression: true, MaxResponseHeaderBytes: 64 << 10, ForceAttemptHTTP2: false, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}
	guard := &guardedTransport{transport: transport, endpoint: endpoint, maxRequestBytes: gateway.options.MaxRequestBytes, maxResponseBytes: gateway.options.MaxResponseBytes, method: method, contentType: contentType}
	return &http.Client{Transport: guard, Timeout: gateway.options.RequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, guard, nil
}
