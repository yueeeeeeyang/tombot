package dockernode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (operation roundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return operation(request)
}

func TestEngineProtocolAndFailureRedaction(t *testing.T) {
	var seenMethod, seenPath, seenBody string
	status := 201
	response := `{"Id":"container-id"}`
	client := &http.Client{Timeout: time.Second, Transport: roundTrip(func(request *http.Request) (*http.Response, error) {
		seenMethod, seenPath = request.Method, request.URL.RequestURI()
		if request.Body != nil {
			encoded, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			seenBody = string(encoded)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
	})}
	engine := &dockerEngine{client: client}
	id, err := engine.createContainer(context.Background(), "test name", createContainer{Image: "fixed-image"})
	if err != nil || id != "container-id" || seenMethod != "POST" || seenPath != "/v1.47/containers/create?name=test+name" {
		t.Fatal("节点创建协议错误", err)
	}
	var body createContainer
	if err = json.Unmarshal([]byte(seenBody), &body); err != nil || body.Image != "fixed-image" {
		t.Fatal("节点请求内容错误", err)
	}
	status = 404
	response = `{"message":"sensitive-host-path"}`
	if _, err = engine.inspectContainer(context.Background(), "missing"); !errors.Is(err, errMissing) {
		t.Fatal("不存在未明确分类", err)
	}
	status = 500
	if _, err = engine.inspectContainer(context.Background(), "unknown"); err == nil || strings.Contains(err.Error(), "sensitive-host-path") {
		t.Fatal("节点错误响应泄露")
	}
	status = 200
	response = strings.Repeat("a", (2<<20)+1)
	if _, err = engine.inspectContainer(context.Background(), "large"); err == nil {
		t.Fatal("未限制节点响应大小")
	}
	response = `{} {}`
	if _, err = engine.inspectContainer(context.Background(), "multiple"); err == nil {
		t.Fatal("节点响应可包含多个对象")
	}
	status = 204
	response = ""
	if err = engine.removeVolume(context.Background(), "owned"); err != nil || seenPath != "/v1.47/volumes/owned?force=false" {
		t.Fatal("卷清理使用了强制删除", err)
	}
	if err = engine.removeContainer(context.Background(), "owned"); err != nil || seenPath != "/v1.47/containers/owned?force=false&v=false" {
		t.Fatal("容器清理隐式删除了卷", err)
	}
}

func TestEngineCancellationAndRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", "https://untrusted.example.test")
			w.WriteHeader(302)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: roundTrip(func(request *http.Request) (*http.Response, error) {
		clone := request.Clone(request.Context())
		destination := *request.URL
		destination.Scheme = "http"
		destination.Host = strings.TrimPrefix(server.URL, "http://")
		clone.URL = &destination
		if strings.HasSuffix(destination.Path, "redirect") {
			destination.Path = "/redirect"
		}
		return transport.RoundTrip(clone)
	}), Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	engine := &dockerEngine{client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := engine.request(ctx, "GET", "/wait", nil, nil, 200); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("请求取消未终止节点调用", err)
	}
	if err := engine.request(context.Background(), "GET", "/redirect", nil, nil, 200); err == nil {
		t.Fatal("节点响应允许重定向")
	}
}
