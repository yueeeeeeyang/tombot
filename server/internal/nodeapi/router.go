package nodeapi

import (
	"context"
	"crypto/tls"
	"errors"
	"sync"
	"time"
)

// Endpoint 是登记后不可改写的节点连接身份；运行时不接受业务请求自带 URL。
type Endpoint struct {
	NodeID      string
	URL         string
	Fingerprint string
	ImageID     string
}

type Resolver interface {
	Resolve(context.Context, string) (Endpoint, error)
}

// Router 按请求中的节点 ID 路由，缓存上限与服务准入上限一致，保留各节点独立的连接池。
type Router struct {
	resolver    Resolver
	certificate tls.Certificate
	timeout     time.Duration
	maximum     int
	mutex       sync.RWMutex
	clients     map[string]*Client
}

func NewRouter(resolver Resolver, certificate tls.Certificate, timeout time.Duration, maximum int) (*Router, error) {
	if resolver == nil || !validTimeout(timeout) || maximum < 1 || maximum > 1000 {
		return nil, errors.New("多节点路由配置无效")
	}
	if _, err := parseLeaf(certificate); err != nil {
		return nil, err
	}
	return &Router{resolver: resolver, certificate: certificate, timeout: timeout, maximum: maximum, clients: map[string]*Client{}}, nil
}

func (router *Router) client(ctx context.Context, nodeID string) (*Client, error) {
	if !validNode(nodeID) {
		return nil, errors.New("节点路由身份无效")
	}
	router.mutex.RLock()
	client := router.clients[nodeID]
	router.mutex.RUnlock()
	if client != nil {
		return client, nil
	}
	ctx, cancel := context.WithTimeout(ctx, router.timeout)
	defer cancel()
	endpoint, err := router.resolver.Resolve(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if endpoint.NodeID != nodeID {
		return nil, errors.New("节点连接目录归属无效")
	}
	fingerprints, err := ParseFingerprints([]string{endpoint.Fingerprint})
	if err != nil {
		return nil, err
	}
	configuration, err := ClientTLS(router.certificate, fingerprints)
	if err != nil {
		return nil, err
	}
	client, err = NewClient(ClientOptions{NodeID: nodeID, URL: endpoint.URL, CallTimeout: router.timeout, TLS: configuration})
	if err != nil {
		return nil, err
	}
	router.mutex.Lock()
	defer router.mutex.Unlock()
	if existing := router.clients[nodeID]; existing != nil {
		return existing, nil
	}
	if len(router.clients) >= router.maximum {
		return nil, errors.New("节点连接缓存达到准入上限")
	}
	router.clients[nodeID] = client
	return client, nil
}

func (router *Router) CloseIdle() error {
	router.mutex.RLock()
	defer router.mutex.RUnlock()
	var failure error
	for _, client := range router.clients {
		failure = errors.Join(failure, client.CloseIdle())
	}
	return failure
}

func routeCall[T any](ctx context.Context, router *Router, nodeID, action string, input any) (T, error) {
	client, err := router.client(ctx, nodeID)
	if err != nil {
		var empty T
		return empty, err
	}
	return call[T](ctx, client, action, input)
}
