package computeragent

import (
	"errors"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/egressproxy"
)

const ControlDirectory = "/run/tombot-control"
const ConfigName = "config.json"
const SocketName = "agent.sock"
const EgressSocketName = "egress.sock"

// ConfigurationVersion 阻止忽略共享浏览器关闭记录的旧代理接管新实例。
const ConfigurationVersion = 4

// RuntimeOptions 由节点受保护配置提供，时限使用毫秒，容器不能从业务请求覆盖这些值。
type RuntimeOptions struct {
	CallTimeoutMS              int   `json:"call_timeout_ms"`
	CleanupTimeoutMS           int   `json:"cleanup_timeout_ms"`
	RetryDelayMS               int   `json:"retry_delay_ms"`
	CleanupAttempts            int   `json:"cleanup_attempts"`
	PollIntervalMS             int   `json:"poll_interval_ms"`
	ShutdownTimeoutMS          int   `json:"shutdown_timeout_ms"`
	MaximumActive              int   `json:"maximum_active"`
	MaximumConnections         int   `json:"maximum_connections"`
	EgressConnectTimeoutMS     int   `json:"egress_connect_timeout_ms"`
	EgressIdleTimeoutMS        int   `json:"egress_idle_timeout_ms"`
	EgressLifetimeMS           int   `json:"egress_lifetime_ms"`
	EgressMaximumConnections   int   `json:"egress_maximum_connections"`
	EgressMaximumTransferBytes int64 `json:"egress_maximum_transfer_bytes"`
}

func (options RuntimeOptions) Validate() error {
	if options.MaximumActive < 1 || options.MaximumActive > computer.MaxConcurrentDesktops || options.CallTimeoutMS < 1000 || options.CallTimeoutMS > 60000 || options.CleanupTimeoutMS < 1000 || options.CleanupTimeoutMS > 60000 || options.RetryDelayMS < 10 || options.RetryDelayMS > 1000 || options.CleanupAttempts < 1 || options.CleanupAttempts > 5 || options.PollIntervalMS < 1 || options.PollIntervalMS > 100 || options.ShutdownTimeoutMS < options.CallTimeoutMS+2*options.CleanupTimeoutMS || options.ShutdownTimeoutMS > 180000 || options.MaximumConnections < 1 || options.MaximumConnections > 128 {
		return errors.New("电脑代理运行配置无效")
	}
	// 先验证毫秒原值，防止整数乘法溢出后变成看似合法的短时限。
	if options.EgressConnectTimeoutMS < 1000 || options.EgressConnectTimeoutMS > 60000 || options.EgressIdleTimeoutMS < 1000 || options.EgressIdleTimeoutMS > 60000 || options.EgressLifetimeMS < 1000 || options.EgressLifetimeMS > 3600000 {
		return errors.New("电脑代理出网时限无效")
	}
	return options.egressOptions().Validate()
}

func (options RuntimeOptions) egressOptions() egressproxy.BridgeOptions {
	return egressproxy.BridgeOptions{ConnectTimeout: time.Duration(options.EgressConnectTimeoutMS) * time.Millisecond, IdleTimeout: time.Duration(options.EgressIdleTimeoutMS) * time.Millisecond, Lifetime: time.Duration(options.EgressLifetimeMS) * time.Millisecond, ShutdownTimeout: time.Duration(options.ShutdownTimeoutMS) * time.Millisecond, MaximumConnections: options.EgressMaximumConnections, MaximumTransferBytes: options.EgressMaximumTransferBytes}
}

func (options RuntimeOptions) guardOptions() Options {
	return Options{CallTimeout: time.Duration(options.CallTimeoutMS) * time.Millisecond, CleanupTimeout: time.Duration(options.CleanupTimeoutMS) * time.Millisecond, RetryDelay: time.Duration(options.RetryDelayMS) * time.Millisecond, CleanupAttempts: options.CleanupAttempts}
}

// Configuration 固定容器实例及运行限制，不包含用户命令、凭据或任意宿主路径。
type Configuration struct {
	Version  int            `json:"version"`
	Identity Identity       `json:"identity"`
	Options  RuntimeOptions `json:"options"`
}

func (configuration Configuration) Validate() error {
	if configuration.Version != ConfigurationVersion || configuration.Identity.validate() != nil {
		return errors.New("电脑代理部署实例配置无效")
	}
	return configuration.Options.Validate()
}
