// Package dockernode 在受信节点中执行限定的 Docker 电脑生命周期，不向业务服务暴露通用 Docker API。
package dockernode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var errMissing = errors.New("节点资源不存在")

// engineError 只保留状态分类，不泄露守护进程路径、命令、环境或原始响应。
type engineError struct{ status int }

func (failure *engineError) Error() string { return "Docker 节点请求失败" }

type dockerEngine struct {
	client    *http.Client
	transport *http.Transport
}

// newEngine 仅连接部署明确指定的本机 Unix socket，不读取 Docker CLI 配置或环境代理。
func newEngine(socket string, timeout time.Duration) (*dockerEngine, error) {
	if !filepath.IsAbs(socket) || timeout < time.Second || timeout > 5*time.Minute {
		return nil, errors.New("Docker 节点连接配置无效")
	}
	stat, err := os.Lstat(socket)
	if err != nil || stat.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("Docker 节点套接字不可用")
	}
	dialer := net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", socket)
	},
		MaxIdleConns: 8, MaxIdleConnsPerHost: 8, MaxConnsPerHost: 32, IdleConnTimeout: time.Minute, ResponseHeaderTimeout: timeout, DisableCompression: true}
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &dockerEngine{client, transport}, nil
}

func (engine *dockerEngine) close() { engine.transport.CloseIdleConnections() }

func (engine *dockerEngine) request(ctx context.Context, method, path string, input, output any, accepted ...int) (result error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return errors.New("编码节点请求失败")
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://docker/v1.47"+path, body)
	if err != nil {
		return errors.New("构造节点请求失败")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := engine.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Docker 节点连接失败，执行结果需要对账")
	}
	defer func() {
		if response.Body.Close() != nil {
			result = errors.Join(result, errors.New("关闭节点响应失败"))
		}
	}()
	const maximum = 2 << 20
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil || len(encoded) > maximum {
		return errors.New("读取节点响应失败或响应过大")
	}
	if response.StatusCode == 404 {
		return errMissing
	}
	allowed := false
	for _, status := range accepted {
		if status == response.StatusCode {
			allowed = true
		}
	}
	if !allowed {
		return &engineError{status: response.StatusCode}
	}
	if output == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if err = decoder.Decode(output); err != nil {
		return errors.New("节点响应格式无效")
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("节点响应包含多份内容")
	}
	return nil
}

type volume struct {
	Name    string
	Driver  string
	Labels  map[string]string
	Options map[string]string
}
type container struct {
	ID     string `json:"Id"`
	Name   string
	Config struct {
		Image      string
		User       string
		Labels     map[string]string
		Entrypoint []string
		Cmd        []string
		Env        []string
	}
	HostConfig hostConfig
	State      struct {
		Running  bool
		Dead     bool
		Status   string
		Pid      int
		ExitCode int
	}
	Mounts          []mountedResource
	NetworkSettings struct {
		Networks map[string]networkEndpoint
	}
}
type networkEndpoint struct {
	NetworkID         string
	IPAddress         string
	GlobalIPv6Address string
	Gateway           string
	IPv6Gateway       string
}
type mountedResource struct {
	Type        string
	Name        string
	Source      string
	Destination string
	RW          bool
}
type mount struct {
	Type          string
	Source        string
	Target        string
	ReadOnly      bool
	VolumeOptions *volumeMountOptions `json:",omitempty"`
}
type volumeMountOptions struct {
	NoCopy  bool
	Subpath string `json:",omitempty"`
}
type hostConfig struct {
	NetworkMode     string
	ReadonlyRootfs  bool
	Privileged      bool
	CapDrop         []string
	CapAdd          []string `json:",omitempty"`
	SecurityOpt     []string
	NanoCpus        int64
	Memory          int64
	MemorySwap      int64
	ShmSize         int64
	PidsLimit       int64
	Init            *bool
	AutoRemove      bool
	PidMode         string
	IpcMode         string
	CgroupnsMode    string
	Mounts          []mount
	Tmpfs           map[string]string
	PortBindings    map[string]any
	PublishAllPorts bool
	RestartPolicy   struct{ Name string }
	LogConfig       struct {
		Type   string
		Config map[string]string
	}
}
type createContainer struct {
	Image      string
	User       string
	Hostname   string
	WorkingDir string
	Entrypoint []string
	Cmd        []string
	Env        []string
	Labels     map[string]string
	StopSignal string
	HostConfig hostConfig
}

// engine 的创建参数只能来自节点固定配置和已验证的资源身份；不导出任意 exec、挂载或端口入口。
type engine interface {
	inspectImage(context.Context, string) (imageDescription, error)
	inspectVolume(context.Context, string) (volume, error)
	createVolume(context.Context, string, string, map[string]string) (volume, error)
	removeVolume(context.Context, string) error
	inspectContainer(context.Context, string) (container, error)
	createContainer(context.Context, string, createContainer) (string, error)
	startContainer(context.Context, string) error
	stopContainer(context.Context, string) error
	removeContainer(context.Context, string) error
}

func (engine *dockerEngine) inspectVolume(ctx context.Context, name string) (volume, error) {
	var value volume
	err := engine.request(ctx, "GET", "/volumes/"+url.PathEscape(name), nil, &value, 200)
	return value, err
}
func (engine *dockerEngine) createVolume(ctx context.Context, name, path string, labels map[string]string) (volume, error) {
	input := struct {
		Name       string
		Driver     string
		DriverOpts map[string]string
		Labels     map[string]string
	}{name, "local", map[string]string{"type": "none", "device": path, "o": "bind"}, labels}
	var value volume
	err := engine.request(ctx, "POST", "/volumes/create", input, &value, 201)
	return value, err
}
func (engine *dockerEngine) removeVolume(ctx context.Context, name string) error {
	return engine.request(ctx, "DELETE", "/volumes/"+url.PathEscape(name)+"?force=false", nil, nil, 204)
}
func (engine *dockerEngine) inspectContainer(ctx context.Context, id string) (container, error) {
	var value container
	err := engine.request(ctx, "GET", "/containers/"+url.PathEscape(id)+"/json", nil, &value, 200)
	return value, err
}
func (engine *dockerEngine) createContainer(ctx context.Context, name string, configuration createContainer) (string, error) {
	var response struct {
		ID       string `json:"Id"`
		Warnings []string
	}
	err := engine.request(ctx, "POST", "/containers/create?name="+url.QueryEscape(name), configuration, &response, 201)
	if err == nil && len(response.Warnings) > 0 {
		err = errors.New("Docker 容器创建存在警告，需要对账")
	}
	return response.ID, err
}
func (engine *dockerEngine) startContainer(ctx context.Context, id string) error {
	return engine.request(ctx, "POST", "/containers/"+url.PathEscape(id)+"/start", nil, nil, 204, 304)
}
func (engine *dockerEngine) stopContainer(ctx context.Context, id string) error {
	return engine.request(ctx, "POST", "/containers/"+url.PathEscape(id)+"/stop?t=10", nil, nil, 204, 304)
}
func (engine *dockerEngine) removeContainer(ctx context.Context, id string) error {
	return engine.request(ctx, "DELETE", "/containers/"+url.PathEscape(id)+"?force=false&v=false", nil, nil, 204)
}

func sameLabels(actual, expected map[string]string) bool {
	for key, value := range expected {
		if actual[key] != value {
			return false
		}
	}
	for key := range actual {
		if strings.HasPrefix(key, "io.tombot.") && actual[key] != expected[key] {
			return false
		}
	}
	return true
}
