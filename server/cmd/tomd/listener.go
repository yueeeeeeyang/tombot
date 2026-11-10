package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"

	"io.yueyang/tom/internal/config"
)

// loadServerTLS 在打开实例目录之前验证证书和私钥是否成对，配置有误时阻止监听。
func loadServerTLS(cfg config.Config) (*tls.Config, error) {
	if cfg.TLSCertFile == "" && cfg.TLSKeyFile == "" {
		return nil, nil
	}
	pair, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("无法加载服务端 TLS 证书与私钥：%w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}, nil
}

// serveListener 将监听器交给 HTTP 服务。启用 TLS 时所有路由共用同一 HTTPS 入口。
func serveListener(server *http.Server, listener net.Listener) (result error) {
	defer func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			result = errors.Join(result, err)
		}
	}()
	if server.TLSConfig != nil {
		return server.ServeTLS(listener, "", "")
	}
	return server.Serve(listener)
}
