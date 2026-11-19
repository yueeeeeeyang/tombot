package egressproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type authorizedKey struct{}

func (proxy *Server) handle(w http.ResponseWriter, r *http.Request) {
	proxy.pending.Add(1)
	defer proxy.pending.Done()
	if allowed, _ := r.Context().Value(authorizedKey{}).(bool); !allowed {
		proxy.failure(w, r, 403, "连接身份未通过校验")
		return
	}
	if r.ProtoMajor != 1 {
		proxy.failure(w, r, 400, "代理仅接受 HTTP/1 请求")
		return
	}
	if r.Method == http.MethodConnect {
		proxy.tunnel(w, r)
		return
	}
	if r.URL == nil || r.URL.Scheme != "http" || r.URL.User != nil || r.URL.Opaque != "" || r.URL.Fragment != "" || r.URL.Host == "" || r.Header.Get("Upgrade") != "" {
		proxy.failure(w, r, 400, "代理请求格式无效")
		return
	}
	destination, err := target(r.URL.Host, "80")
	claimed, claimErr := target(r.Host, "80")
	if err != nil || claimErr != nil || destination != claimed {
		proxy.failure(w, r, 400, "代理目标不一致")
		return
	}
	if r.ContentLength > proxy.options.MaximumRequestBytes {
		proxy.failure(w, r, 413, "请求正文超过限额")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), proxy.options.RequestTimeout)
	defer cancel()
	outgoing := r.Clone(ctx)
	outgoing.RequestURI = ""
	outgoing.GetBody = nil
	outgoing.Close = true
	stripHopHeaders(outgoing.Header)
	outgoing.Body = http.MaxBytesReader(w, r.Body, proxy.options.MaximumRequestBytes)
	var responseHeaders *headerConnection
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true, ForceAttemptHTTP2: false, ResponseHeaderTimeout: proxy.options.HeaderTimeout, ExpectContinueTimeout: proxy.options.HeaderTimeout, MaxResponseHeaderBytes: 32 << 10,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			expected := net.JoinHostPort(destination.host, destination.port)
			actual, err := target(address, "")
			if network != "tcp" || err != nil || net.JoinHostPort(actual.host, actual.port) != expected {
				return nil, ErrAddressDenied
			}
			connection, err := proxy.connect(ctx, destination)
			if err != nil {
				return nil, err
			}
			responseHeaders = &headerConnection{Conn: connection}
			return responseHeaders, nil
		}}
	defer transport.CloseIdleConnections()
	response, err := transport.RoundTrip(outgoing)
	if err != nil {
		var limit *http.MaxBytesError
		status := 502
		if errors.Is(err, ErrAddressDenied) {
			status = 403
		} else if errors.As(err, &limit) {
			status = 413
		}
		proxy.failure(w, r, status, "出网请求失败或被策略拒绝")
		return
	}
	defer func() {
		if response.Body.Close() != nil {
			proxy.logFailure(r, "response_close_failed")
		}
	}()
	if response.StatusCode == http.StatusSwitchingProtocols || response.ContentLength > proxy.options.MaximumResponseBytes {
		proxy.failure(w, r, 502, "目标响应不符合传输限制")
		return
	}
	stripHopHeaders(response.Header)
	if responseHeaders != nil {
		for _, name := range responseHeaders.hopHeaders {
			response.Header.Del(name)
		}
	}
	for name, values := range response.Header {
		w.Header()[name] = append([]string(nil), values...)
	}
	w.Header().Set("Connection", "close")
	w.WriteHeader(response.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	count, err := io.Copy(w, io.LimitReader(response.Body, proxy.options.MaximumResponseBytes))
	if err == nil && count == proxy.options.MaximumResponseBytes {
		var extra [1]byte
		n, readErr := response.Body.Read(extra[:])
		if n > 0 {
			err = ErrLimit
		} else if readErr != nil && !errors.Is(readErr, io.EOF) {
			err = readErr
		}
	}
	if err != nil {
		proxy.logFailure(r, "response_transfer_incomplete")
		panic(http.ErrAbortHandler)
	}
}

func stripHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(name)
	}
}

func (proxy *Server) tunnel(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil || r.URL.Scheme != "" || r.URL.User != nil || r.URL.Path != "" || r.URL.RawQuery != "" || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		proxy.failure(w, r, 400, "隧道请求格式无效")
		return
	}
	destination, err := target(r.RequestURI, "")
	claimed, claimErr := target(r.Host, "")
	if err != nil || claimErr != nil || destination != claimed {
		proxy.failure(w, r, 400, "隧道目标不一致")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), proxy.options.TunnelTimeout)
	defer cancel()
	upstream, err := proxy.connect(ctx, destination)
	if err != nil {
		status := 502
		if errors.Is(err, ErrAddressDenied) {
			status = 403
		}
		proxy.failure(w, r, status, "隧道目标不可用或不允许")
		return
	}
	defer func() {
		if err := upstream.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			proxy.logFailure(r, "upstream_close_failed")
		}
	}()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		proxy.failure(w, r, 500, "隧道连接类型不支持")
		return
	}
	connection, buffer, err := hijacker.Hijack()
	if err != nil {
		proxy.logFailure(r, "tunnel_hijack_failed")
		return
	}
	defer func() {
		if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			proxy.logFailure(r, "client_close_failed")
		}
	}()
	deadline, _ := ctx.Deadline()
	if err := connection.SetDeadline(time.Now().Add(proxy.options.HeaderTimeout)); err != nil {
		proxy.logFailure(r, "tunnel_deadline_failed")
		return
	}
	if _, err := buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		proxy.logFailure(r, "tunnel_ack_failed")
		return
	}
	if err := buffer.Flush(); err != nil {
		proxy.logFailure(r, "tunnel_ack_failed")
		return
	}
	client := &timedConnection{Conn: connection, idle: proxy.options.IdleTimeout, deadline: deadline}
	// 保留 Hijack 之前已读取的字节，TLS ClientHello 可以与 CONNECT 同批到达。
	reader := io.MultiReader(io.LimitReader(buffer.Reader, int64(buffer.Reader.Buffered())), client)
	if err := relay(ctx, client, reader, upstream, proxy.options.MaximumTunnelBytes); err != nil {
		proxy.logFailure(r, "tunnel_incomplete")
	}
}

// relay 每方向都有独立硬上限；关闭或取消唤醒双方复制，不遗留隧道工作者。
func relay(ctx context.Context, client net.Conn, reader io.Reader, upstream net.Conn, maximum int64) error {
	results := make(chan error, 2)
	copyOne := func(destination net.Conn, source io.Reader) {
		count, err := io.Copy(destination, io.LimitReader(source, maximum))
		if err == nil && count == maximum {
			err = ErrLimit
		}
		if err == nil {
			underlying := destination
			if wrapped, ok := destination.(*timedConnection); ok {
				underlying = wrapped.Conn
			}
			if half, ok := underlying.(interface{ CloseWrite() error }); ok {
				err = half.CloseWrite()
			}
		}
		results <- err
	}
	go copyOne(upstream, reader)
	go copyOne(client, upstream)
	closed := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { closed <- closeNetworkConnections(client, upstream) })
	var result error
	for range 2 {
		err := <-results
		if err != nil {
			result = errors.Join(result, err, closeNetworkConnections(client, upstream))
		}
	}
	if !stop() {
		result = errors.Join(result, <-closed)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return result
}

func (proxy *Server) failure(w http.ResponseWriter, r *http.Request, status int, message string) {
	w.Header().Set("Connection", "close")
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, message, status)
	proxy.logFailure(r, "request_rejected")
}
func (proxy *Server) logFailure(r *http.Request, code string) {
	proxy.logger.WarnContext(r.Context(), "电脑出网请求未完成", "user_id", proxy.owner.UserID, "computer_id", proxy.owner.ComputerID, "reason", code)
}
