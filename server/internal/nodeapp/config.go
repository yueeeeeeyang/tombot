// Package nodeapp 组合受信宿主节点，不持有业务数据库或模型凭据。
package nodeapp

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/dockernode"
	"io.yueyang/tom/internal/egressproxy"
	"io.yueyang/tom/internal/nodeapi"
	"io.yueyang/tom/internal/processgroup"
)

// Milliseconds 先限制原值再转换，避免配置整数溢出后被当成合法时限。
type Milliseconds int64

func (value Milliseconds) duration() time.Duration {
	if value <= 0 || value > 3600000 {
		return 0
	}
	return time.Duration(value) * time.Millisecond
}

type Config struct {
	Version         int                          `json:"version"`
	NodeID          string                       `json:"node_id"`
	Listen          string                       `json:"listen"`
	AllowPublic     bool                         `json:"allow_public_listen"`
	Control         ControlTLS                   `json:"control_tls"`
	StartupTimeout  Milliseconds                 `json:"startup_timeout_ms"`
	ShutdownTimeout Milliseconds                 `json:"shutdown_timeout_ms"`
	API             APIConfig                    `json:"api"`
	Runtime         RuntimeConfig                `json:"runtime"`
	Storage         StorageConfig                `json:"storage"`
	Groups          GroupConfig                  `json:"groups"`
	Sessions        SessionConfig                `json:"sessions"`
	Probe           ProbeConfig                  `json:"probe"`
	Browser         BrowserConfig                `json:"browser"`
	Agent           computeragent.RuntimeOptions `json:"agent"`
	AgentStartup    AgentStartupConfig           `json:"agent_startup"`
	Egress          EgressConfig                 `json:"egress"`
}

// ControlTLS 固定本机证书与允许的 API 客户端指纹；节点不再依赖同机内核凭据。
type ControlTLS struct {
	CertificateFile    string   `json:"certificate_file"`
	PrivateKeyFile     string   `json:"private_key_file"`
	ClientFingerprints []string `json:"client_fingerprints"`
}

type APIConfig struct {
	CallTimeout        Milliseconds `json:"call_timeout_ms"`
	ShutdownTimeout    Milliseconds `json:"shutdown_timeout_ms"`
	MaximumConnections int          `json:"maximum_connections"`
}
type RuntimeConfig struct {
	Socket                 string       `json:"docker_socket"`
	StateDirectory         string       `json:"state_directory"`
	StorageDirectory       string       `json:"storage_directory"`
	CallTimeout            Milliseconds `json:"call_timeout_ms"`
	MaximumLease           Milliseconds `json:"maximum_lease_ms"`
	TemporaryBytes         int64        `json:"temporary_bytes"`
	RuntimeBytes           int64        `json:"runtime_bytes"`
	SharedMemoryBytes      int64        `json:"shared_memory_bytes"`
	MaximumEgressComputers int          `json:"maximum_egress_computers"`
}
type StorageConfig struct {
	StateDirectory   string       `json:"state_directory"`
	CommandTimeout   Milliseconds `json:"command_timeout_ms"`
	MinimumFreeBytes uint64       `json:"minimum_free_bytes"`
}
type GroupConfig struct {
	GracePeriod      Milliseconds `json:"grace_period_ms"`
	PollInterval     Milliseconds `json:"poll_interval_ms"`
	MaximumProcesses int          `json:"maximum_processes"`
}
type SessionConfig struct {
	MaximumActive   int          `json:"maximum_active"`
	CallTimeout     Milliseconds `json:"call_timeout_ms"`
	PollInterval    Milliseconds `json:"poll_interval_ms"`
	RetryDelay      Milliseconds `json:"retry_delay_ms"`
	CleanupAttempts int          `json:"cleanup_attempts"`
}
type ProbeConfig struct {
	CallTimeout    Milliseconds `json:"call_timeout_ms"`
	CleanupTimeout Milliseconds `json:"cleanup_timeout_ms"`
}
type AgentStartupConfig struct {
	Timeout         Milliseconds `json:"timeout_ms"`
	CallTimeout     Milliseconds `json:"call_timeout_ms"`
	RetryDelay      Milliseconds `json:"retry_delay_ms"`
	MaximumAttempts int          `json:"maximum_attempts"`
}

func (configuration AgentStartupConfig) options() dockernode.AgentStartupOptions {
	return dockernode.AgentStartupOptions{Timeout: configuration.Timeout.duration(), CallTimeout: configuration.CallTimeout.duration(), RetryDelay: configuration.RetryDelay.duration(), MaximumAttempts: configuration.MaximumAttempts}
}

type BrowserConfig struct {
	CallTimeout     Milliseconds                `json:"call_timeout_ms"`
	CleanupTimeout  Milliseconds                `json:"cleanup_timeout_ms"`
	ShutdownTimeout Milliseconds                `json:"shutdown_timeout_ms"`
	Limits          computeragent.BrowserLimits `json:"limits"`
}
type EgressConfig struct {
	Ports                []uint16       `json:"ports"`
	DeniedNetworks       []netip.Prefix `json:"denied_networks"`
	ConnectTimeout       Milliseconds   `json:"connect_timeout_ms"`
	HeaderTimeout        Milliseconds   `json:"header_timeout_ms"`
	IdleTimeout          Milliseconds   `json:"idle_timeout_ms"`
	RequestTimeout       Milliseconds   `json:"request_timeout_ms"`
	TunnelTimeout        Milliseconds   `json:"tunnel_timeout_ms"`
	ShutdownTimeout      Milliseconds   `json:"shutdown_timeout_ms"`
	MaximumConnections   int            `json:"maximum_connections"`
	MaximumRequestBytes  int64          `json:"maximum_request_bytes"`
	MaximumResponseBytes int64          `json:"maximum_response_bytes"`
	MaximumTunnelBytes   int64          `json:"maximum_tunnel_bytes"`
}

func (configuration Config) apiOptions() nodeapi.ServerOptions {
	return nodeapi.ServerOptions{NodeID: configuration.NodeID, CallTimeout: configuration.API.CallTimeout.duration(), ShutdownTimeout: configuration.API.ShutdownTimeout.duration(), MaximumConnections: configuration.API.MaximumConnections}
}
func (configuration GroupConfig) options() processgroup.Options {
	return processgroup.Options{GracePeriod: configuration.GracePeriod.duration(), PollInterval: configuration.PollInterval.duration(), MaximumProcesses: configuration.MaximumProcesses}
}
func (configuration SessionConfig) options() dockernode.SessionOptions {
	return dockernode.SessionOptions{MaximumActive: configuration.MaximumActive, CallTimeout: configuration.CallTimeout.duration(), PollInterval: configuration.PollInterval.duration(), RetryDelay: configuration.RetryDelay.duration(), MaxCleanupAttempts: configuration.CleanupAttempts}
}
func (configuration Config) storageOptions() dockernode.FileStorageOptions {
	return dockernode.FileStorageOptions{Directory: configuration.Runtime.StorageDirectory, StateDirectory: configuration.Storage.StateDirectory, CommandTimeout: configuration.Storage.CommandTimeout.duration(), MinimumFreeBytes: configuration.Storage.MinimumFreeBytes}
}
func (configuration EgressConfig) options() egressproxy.Options {
	return egressproxy.Options{Ports: configuration.Ports, DeniedNetworks: configuration.DeniedNetworks, ConnectTimeout: configuration.ConnectTimeout.duration(), HeaderTimeout: configuration.HeaderTimeout.duration(), IdleTimeout: configuration.IdleTimeout.duration(), RequestTimeout: configuration.RequestTimeout.duration(), TunnelTimeout: configuration.TunnelTimeout.duration(), ShutdownTimeout: configuration.ShutdownTimeout.duration(), MaximumConnections: configuration.MaximumConnections, MaximumRequestBytes: configuration.MaximumRequestBytes, MaximumResponseBytes: configuration.MaximumResponseBytes, MaximumTunnelBytes: configuration.MaximumTunnelBytes}
}
func (configuration Config) runtimeOptions() dockernode.Options {
	value := configuration.Runtime
	return dockernode.Options{NodeID: configuration.NodeID, Socket: value.Socket, StateDirectory: value.StateDirectory, StorageDirectory: value.StorageDirectory, CallTimeout: value.CallTimeout.duration(), MaximumLease: value.MaximumLease.duration(), TemporaryBytes: value.TemporaryBytes, RuntimeBytes: value.RuntimeBytes, SharedMemoryBytes: value.SharedMemoryBytes, Agent: configuration.Agent, AgentStartup: configuration.AgentStartup.options(), Probes: dockernode.ProbeOptions{CallTimeout: configuration.Probe.CallTimeout.duration(), CleanupTimeout: configuration.Probe.CleanupTimeout.duration(), Groups: configuration.Groups.options()}, Browser: dockernode.BrowserOptions{CallTimeout: configuration.Browser.CallTimeout.duration(), CleanupTimeout: configuration.Browser.CleanupTimeout.duration(), ShutdownTimeout: configuration.Browser.ShutdownTimeout.duration(), Groups: configuration.Groups.options(), Limits: configuration.Browser.Limits}, Egress: configuration.Egress.options(), MaximumEgressComputers: value.MaximumEgressComputers}
}

// Validate 复用各领域的权威参数校验，并检查组件之间的时限和目录关系。
func (configuration Config) Validate() error {
	if configuration.Version != 2 {
		return errors.New("节点配置版本无效")
	}
	for _, err := range []error{configuration.apiOptions().Validate(), configuration.runtimeOptions().Validate(), configuration.storageOptions().Validate(), configuration.Sessions.options().Validate(), configuration.Groups.options().Validate()} {
		if err != nil {
			return err
		}
	}
	if configuration.StartupTimeout.duration() < configuration.Sessions.CallTimeout.duration()+configuration.Runtime.CallTimeout.duration() || configuration.StartupTimeout.duration() > 5*time.Minute || configuration.ShutdownTimeout.duration() < configuration.API.ShutdownTimeout.duration()+configuration.Sessions.CallTimeout.duration()+configuration.Egress.ShutdownTimeout.duration()+configuration.Browser.ShutdownTimeout.duration() || configuration.ShutdownTimeout.duration() > 10*time.Minute {
		return errors.New("节点启动或退出预算不足或超过限制")
	}
	if configuration.API.CallTimeout < configuration.Sessions.CallTimeout || configuration.API.CallTimeout < configuration.Probe.CallTimeout || configuration.API.CallTimeout < configuration.Runtime.CallTimeout || configuration.Sessions.CallTimeout.duration() < time.Duration(configuration.Agent.CallTimeoutMS)*time.Millisecond || configuration.API.ShutdownTimeout.duration() < configuration.Probe.CleanupTimeout.duration()+configuration.Egress.ShutdownTimeout.duration() {
		return errors.New("节点组件调用与回收时限不匹配")
	}
	if configuration.API.CallTimeout < configuration.Browser.CallTimeout || configuration.API.ShutdownTimeout.duration() < configuration.Browser.CleanupTimeout.duration()+configuration.Egress.ShutdownTimeout.duration() {
		return errors.New("节点共享浏览器调用或退出预算不足")
	}
	if configuration.API.CallTimeout.duration() < configuration.AgentStartup.Timeout.duration()+configuration.Browser.CallTimeout.duration()+configuration.Probe.CallTimeout.duration() {
		return errors.New("节点调用预算不足以确认代理、浏览器与基础探针")
	}
	if !validListen(configuration.Listen) || strings.ContainsAny(configuration.Runtime.Socket, "\x00\r\n") {
		return errors.New("节点控制监听地址或 Docker 入口无效")
	}
	if _, err := nodeapi.ParseFingerprints(configuration.Control.ClientFingerprints); err != nil {
		return err
	}
	for _, path := range []string{configuration.Control.CertificateFile, configuration.Control.PrivateKeyFile} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsAny(path, "\x00\r\n") {
			return errors.New("节点控制证书路径无效")
		}
	}
	directories := []string{configuration.Runtime.StateDirectory, configuration.Runtime.StorageDirectory, configuration.Storage.StateDirectory}
	for index, directory := range directories {
		if directory == "/" || strings.ContainsAny(directory, "\x00\r\n") {
			return errors.New("节点数据目录无效")
		}
		if within(configuration.Control.CertificateFile, directory) || within(configuration.Control.PrivateKeyFile, directory) || within(configuration.Runtime.Socket, directory) {
			return errors.New("节点或 Docker 控制入口不得放入私有数据目录")
		}
		for _, other := range directories[index+1:] {
			if within(directory, other) || within(other, directory) {
				return errors.New("节点、磁盘控制记录与用户存储目录不得重叠")
			}
		}
	}
	return nil
}

// validListen 只接受 host:port 形式；是否允许公网地址在监听时再按显式开关判断。
func validListen(value string) bool {
	host, port, err := net.SplitHostPort(value)
	if err != nil || host == "" || port == "" || strings.ContainsAny(value, "\x00\r\n") || len(value) > 255 {
		return false
	}
	number, err := strconv.Atoi(port)
	return err == nil && number > 0 && number < 65536
}

func within(value, directory string) bool {
	return value == directory || strings.HasPrefix(value, directory+string(filepath.Separator))
}

// LoadConfig 仅用于 Linux root 进程；配置文件与父目录复用部署文件的保护规则。
func LoadConfig(path string) (Config, error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return Config{}, errors.New("宿主节点配置只能由 Linux root 服务读取")
	}
	encoded, err := dockernode.ReadDeploymentFile(path)
	if err != nil {
		return Config{}, err
	}
	return ParseConfig(encoded)
}

// ParseConfig 只解析及校验节点参数；供实际入口与部署核对共用，不访问 Docker 或宿主状态。
func ParseConfig(encoded []byte) (Config, error) {
	var configuration Config
	if err := decodeConfig(encoded, &configuration); err != nil {
		return Config{}, err
	}
	if err := configuration.Validate(); err != nil {
		return Config{}, err
	}
	return configuration, nil
}
