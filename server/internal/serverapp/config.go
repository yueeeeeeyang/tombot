// Package serverapp 组合多用户服务的配置、领域模块和进程生命周期，不承载业务规则。
package serverapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"io.yueyang/tom/internal/accounts"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/execution"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/nodeapi"
	"io.yueyang/tom/internal/nodecatalog"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/userevents"
)

// Duration 在部署 JSON 中使用带单位的字符串，避免秒与纳秒混用。
type Duration time.Duration

func (value Duration) Value() time.Duration         { return time.Duration(value) }
func (value Duration) MarshalJSON() ([]byte, error) { return json.Marshal(value.Value().String()) }
func (value *Duration) UnmarshalJSON(encoded []byte) error {
	var text string
	if json.Unmarshal(encoded, &text) != nil {
		return errors.New("配置时限必须使用带单位的字符串")
	}
	parsed, err := time.ParseDuration(text)
	if err != nil || parsed <= 0 || parsed > 90*24*time.Hour {
		return errors.New("配置时限无效")
	}
	*value = Duration(parsed)
	return nil
}

type Config struct {
	Address            string            `json:"address"`
	AdminOrigin        string            `json:"admin_origin"`
	AdminDirectory     string            `json:"admin_directory"`
	TLSCertificateFile string            `json:"tls_certificate_file"`
	TLSPrivateKeyFile  string            `json:"tls_private_key_file"`
	TrustedProxies     []netip.Prefix    `json:"trusted_proxies"`
	IdentityKeyFile    string            `json:"identity_key_file"`
	CSRFKeyFile        string            `json:"csrf_key_file"`
	ActiveModelKey     string            `json:"active_model_key"`
	ModelKeyFiles      map[string]string `json:"model_key_files"`
	Database           DatabaseConfig    `json:"database"`
	Node               NodeConfig        `json:"node"`
	HTTP               HTTPConfig        `json:"http"`
	Identity           IdentityConfig    `json:"identity"`
	Models             ModelConfig       `json:"models"`
	Provision          ProvisionConfig   `json:"provision"`
	Runs               RunConfig         `json:"runs"`
	Computer           ComputerConfig    `json:"computer"`
	Events             EventsConfig      `json:"events"`
	MaxBots            int               `json:"max_bots"`
	MaxGroups          int               `json:"max_groups"`
}
type DatabaseConfig struct {
	DSNFiles           map[postgres.Role]string `json:"dsn_files"`
	MaxConnections     int                      `json:"max_connections"`
	ConnectTimeout     Duration                 `json:"connect_timeout"`
	StatementTimeout   Duration                 `json:"statement_timeout"`
	LockTimeout        Duration                 `json:"lock_timeout"`
	TransactionTimeout Duration                 `json:"transaction_timeout"`
	ConnectionLifetime Duration                 `json:"connection_lifetime"`
}
type NodeConfig struct {
	ID                 string             `json:"id"`
	URL                string             `json:"url"`
	CertificateFile    string             `json:"certificate_file"`
	PrivateKeyFile     string             `json:"private_key_file"`
	ServerFingerprints []string           `json:"server_fingerprints"`
	CallTimeout        Duration           `json:"call_timeout"`
	HeartbeatInterval  Duration           `json:"heartbeat_interval"`
	ControlLifetime    Duration           `json:"control_lifetime"`
	Verification       VerificationConfig `json:"verification"`
}

type VerificationConfig struct {
	ImageID       string             `json:"image_id"`
	Resources     computer.Resources `json:"resources"`
	LeaseDuration Duration           `json:"lease_duration"`
	Interval      Duration           `json:"interval"`
	Lifetime      Duration           `json:"lifetime"`
	RetryDelay    Duration           `json:"retry_delay"`
	MaxAttempts   int                `json:"max_attempts"`
}

func (configuration NodeConfig) verificationOptions() nodecatalog.VerificationOptions {
	value := configuration.Verification
	return nodecatalog.VerificationOptions{ImageID: value.ImageID, Resources: value.Resources, CallTimeout: configuration.CallTimeout.Value(), LeaseDuration: value.LeaseDuration.Value(), Interval: value.Interval.Value(), Lifetime: value.Lifetime.Value(), RetryDelay: value.RetryDelay.Value(), MaxAttempts: value.MaxAttempts}
}

func (configuration NodeConfig) observerOptions() nodecatalog.ObserverOptions {
	return nodecatalog.ObserverOptions{CallTimeout: configuration.CallTimeout.Value(), Interval: configuration.HeartbeatInterval.Value(), Lifetime: configuration.ControlLifetime.Value()}
}

type HTTPConfig struct {
	ReadHeaderTimeout Duration `json:"read_header_timeout"`
	ReadTimeout       Duration `json:"read_timeout"`
	WriteTimeout      Duration `json:"write_timeout"`
	IdleTimeout       Duration `json:"idle_timeout"`
	RequestTimeout    Duration `json:"request_timeout"`
	ShutdownTimeout   Duration `json:"shutdown_timeout"`
}
type IdentityConfig struct {
	AccessLifetime       Duration `json:"access_lifetime"`
	SessionLifetime      Duration `json:"session_lifetime"`
	VerificationLifetime Duration `json:"verification_lifetime"`
	RateWindow           Duration `json:"rate_window"`
	AccountAttempts      int      `json:"account_attempts"`
	SourceAttempts       int      `json:"source_attempts"`
	PasswordWorkers      int      `json:"password_workers"`
	DeviceLimit          int      `json:"device_limit"`
}
type Origin struct {
	URL      string         `json:"url"`
	Networks []netip.Prefix `json:"networks"`
}
type ModelConfig struct {
	Origins               []Origin       `json:"origins"`
	DeniedNetworks        []netip.Prefix `json:"denied_networks"`
	RootCAFile            string         `json:"root_ca_file"`
	ConnectTimeout        Duration       `json:"connect_timeout"`
	TLSHandshakeTimeout   Duration       `json:"tls_handshake_timeout"`
	ResponseHeaderTimeout Duration       `json:"response_header_timeout"`
	IdleReadTimeout       Duration       `json:"idle_read_timeout"`
	WriteTimeout          Duration       `json:"write_timeout"`
	RequestTimeout        Duration       `json:"request_timeout"`
	VerificationTimeout   Duration       `json:"verification_timeout"`
	MaxRequestBytes       int64          `json:"max_request_bytes"`
	MaxResponseBytes      int64          `json:"max_response_bytes"`
	Concurrency           int            `json:"concurrency"`
}
type ProvisionConfig struct {
	Workers            int      `json:"workers"`
	CallTimeout        Duration `json:"call_timeout"`
	LeaseDuration      Duration `json:"lease_duration"`
	OperationTimeout   Duration `json:"operation_timeout"`
	RequestWait        Duration `json:"request_wait"`
	PollInterval       Duration `json:"poll_interval"`
	RetryDelay         Duration `json:"retry_delay"`
	MaxReconciliations int      `json:"max_reconciliations"`
	ActivationLifetime Duration `json:"activation_lifetime"`
	ModelConcurrency   int      `json:"model_concurrency"`
	DesktopConcurrency int      `json:"desktop_concurrency"`
	QueuedRuns         int      `json:"queued_runs"`
}

// options 把部署配置转换为开户领域参数；用户额度与时限均由配置显式提供。
func (provision ProvisionConfig) options() accounts.Options {
	return accounts.Options{WorkerID: ids.New(), Workers: provision.Workers, CallTimeout: provision.CallTimeout.Value(), LeaseDuration: provision.LeaseDuration.Value(), OperationTimeout: provision.OperationTimeout.Value(), RequestWait: provision.RequestWait.Value(), PollInterval: provision.PollInterval.Value(), RetryDelay: provision.RetryDelay.Value(), MaxReconciliations: provision.MaxReconciliations, ActivationLifetime: provision.ActivationLifetime.Value(), ModelConcurrency: provision.ModelConcurrency, DesktopConcurrency: provision.DesktopConcurrency, QueuedRuns: provision.QueuedRuns}
}

type RunConfig struct {
	Concurrency     int         `json:"concurrency"`
	PollInterval    Duration    `json:"poll_interval"`
	RunTimeout      Duration    `json:"run_timeout"`
	ModelTimeout    Duration    `json:"model_timeout"`
	ToolTimeout     Duration    `json:"tool_timeout"`
	RecordTimeout   Duration    `json:"record_timeout"`
	AgentTimeout    Duration    `json:"agent_timeout"`
	MaxModelSteps   int         `json:"max_model_steps"`
	MaxToolCalls    int         `json:"max_tool_calls"`
	MaxOutputBytes  int         `json:"max_output_bytes"`
	LeaseLifetime   Duration    `json:"lease_lifetime"`
	RecoveryDelay   Duration    `json:"recovery_delay"`
	MaxAttempts     int         `json:"max_attempts"`
	MaxOperations   int         `json:"max_operations"`
	ContextMessages int         `json:"context_messages"`
	ContextBytes    int         `json:"context_bytes"`
	Images          ImageConfig `json:"images"`
}
type ImageConfig struct {
	MaxUserBytes  int64    `json:"max_user_bytes"`
	MaxUserImages int      `json:"max_user_images"`
	ContextImages int      `json:"context_images"`
	Retention     Duration `json:"retention"`
}
type ComputerConfig struct {
	LeaseLifetime       Duration `json:"lease_lifetime"`
	WaitTimeout         Duration `json:"wait_timeout"`
	HoldTimeout         Duration `json:"hold_timeout"`
	NodeTimeout         Duration `json:"node_timeout"`
	RetryDelay          Duration `json:"retry_delay"`
	MaxWaiting          int      `json:"max_waiting"`
	MaxCleanupAttempts  int      `json:"max_cleanup_attempts"`
	ShellPollInterval   Duration `json:"shell_poll_interval"`
	DesktopStartTimeout Duration `json:"desktop_start_timeout"`
}

// EventsConfig 控制用户事件保留；事件只是同步提示，快照才是权威状态，因此可以按条数清理。
type EventsConfig struct {
	Retention int      `json:"retention"`
	BatchSize int      `json:"batch_size"`
	Interval  Duration `json:"interval"`
	Timeout   Duration `json:"timeout"`
}

func (configuration EventsConfig) retentionOptions() userevents.RetentionOptions {
	return userevents.RetentionOptions{Retention: configuration.Retention, BatchSize: configuration.BatchSize, Interval: configuration.Interval.Value(), Timeout: configuration.Timeout.Value(), WorkerID: ids.New()}
}

// DefaultConfig 仅提供可覆盖的有界运行参数；地址、数据库、密钥、模型来源和节点身份必须由部署提供。
func DefaultConfig() Config {
	second, minute, hour := Duration(time.Second), Duration(time.Minute), Duration(time.Hour)
	return Config{
		MaxBots: 100, MaxGroups: 100,
		Database:  DatabaseConfig{MaxConnections: 16, ConnectTimeout: 5 * second, StatementTimeout: 10 * second, LockTimeout: 3 * second, TransactionTimeout: 20 * second, ConnectionLifetime: 30 * minute},
		Node:      NodeConfig{CallTimeout: 30 * second, HeartbeatInterval: 10 * second, ControlLifetime: 2 * minute, Verification: VerificationConfig{Resources: computer.Resources{CPUMillis: 1000, MemoryBytes: 2 << 30, DiskBytes: 1 << 30, ProcessLimit: 512}, LeaseDuration: 3 * minute, Interval: 5 * minute, Lifetime: 15 * minute, RetryDelay: 5 * second, MaxAttempts: 5}},
		HTTP:      HTTPConfig{ReadHeaderTimeout: 5 * second, ReadTimeout: 30 * second, WriteTimeout: minute, IdleTimeout: minute, RequestTimeout: minute, ShutdownTimeout: minute},
		Identity:  IdentityConfig{AccessLifetime: 15 * minute, SessionLifetime: 30 * 24 * hour, VerificationLifetime: 15 * minute, RateWindow: 10 * minute, AccountAttempts: 10, SourceAttempts: 100, PasswordWorkers: 2, DeviceLimit: 20},
		Models:    ModelConfig{ConnectTimeout: 5 * second, TLSHandshakeTimeout: 5 * second, ResponseHeaderTimeout: 30 * second, IdleReadTimeout: 30 * second, WriteTimeout: 10 * second, RequestTimeout: 2 * minute, VerificationTimeout: 45 * second, MaxRequestBytes: 16 << 20, MaxResponseBytes: 8 << 20, Concurrency: 16},
		Provision: ProvisionConfig{Workers: 2, CallTimeout: 30 * second, LeaseDuration: 2 * minute, OperationTimeout: 10 * minute, RequestWait: 30 * second, PollInterval: Duration(200 * time.Millisecond), RetryDelay: second, MaxReconciliations: 5, ActivationLifetime: hour, ModelConcurrency: 2, DesktopConcurrency: 2, QueuedRuns: 100},
		Runs:      RunConfig{Concurrency: 4, PollInterval: Duration(200 * time.Millisecond), RunTimeout: 20 * minute, ModelTimeout: 2 * minute, ToolTimeout: 10 * minute, RecordTimeout: 10 * second, AgentTimeout: 5 * second, MaxModelSteps: 32, MaxToolCalls: 8, MaxOutputBytes: 16 << 10, LeaseLifetime: 30 * second, RecoveryDelay: second, MaxAttempts: 3, MaxOperations: 256, ContextMessages: 100, ContextBytes: 512 << 10, Images: ImageConfig{MaxUserBytes: 256 << 20, MaxUserImages: 256, ContextImages: 2, Retention: 7 * 24 * hour}},
		Events:    EventsConfig{Retention: 5000, BatchSize: 200, Interval: 10 * minute, Timeout: 30 * second},
		Computer:  ComputerConfig{LeaseLifetime: 30 * second, WaitTimeout: 5 * minute, HoldTimeout: 25 * minute, NodeTimeout: 5 * second, RetryDelay: second, MaxWaiting: 100, MaxCleanupAttempts: 5, ShellPollInterval: Duration(50 * time.Millisecond), DesktopStartTimeout: 45 * second},
	}
}

// LoadConfig 读取不含凭据值的受保护 JSON；DSN、密钥及 TLS 私钥只允许引用独立私有文件。
func LoadConfig(path string) (Config, error) {
	encoded, err := readFile(path, 128<<10, false)
	if err != nil {
		return Config{}, err
	}
	return ParseConfig(encoded)
}

// ParseConfig 复用服务默认参数与权威校验；只处理配置字节，不读取引用文件或连接外部服务。
func ParseConfig(encoded []byte) (Config, error) {
	if len(encoded) == 0 || len(encoded) > 128<<10 {
		return Config{}, errors.New("服务配置大小无效")
	}
	configuration := DefaultConfig()
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&configuration) != nil || decoder.Decode(new(any)) != io.EOF {
		return Config{}, errors.New("服务配置 JSON 无效或包含未知字段")
	}
	if err := configuration.validate(); err != nil {
		return Config{}, err
	}
	return configuration, nil
}

func (configuration Config) validate() error {
	if _, err := modelgateway.New(configuration.Models.gatewayOptions(nil)); err != nil {
		return errors.New("模型来源、网络范围或请求预算无效")
	}
	if err := configuration.Node.verificationOptions().Validate(); err != nil {
		return err
	}
	if err := configuration.Node.observerOptions().Validate(); err != nil {
		return err
	}
	if err := configuration.Events.retentionOptions().Validate(); err != nil {
		return err
	}
	images := configuration.Runs.Images
	if (execution.ImageOptions{MaxUserBytes: images.MaxUserBytes, MaxUserImages: images.MaxUserImages, ContextImages: images.ContextImages, Retention: images.Retention.Value()}).Validate() != nil || configuration.Computer.DesktopStartTimeout.Value() < time.Second || configuration.Computer.DesktopStartTimeout.Value() > time.Minute {
		return errors.New("桌面截图存储或启动配置无效")
	}
	minimumModelBytes := int64(images.ContextImages)*int64((domain.MaxVisionImageBytes+2)/3*4) + int64(configuration.Runs.ContextBytes)*2 + (64 << 10)
	if configuration.Models.MaxRequestBytes < minimumModelBytes {
		return errors.New("模型请求容量不足以承载配置的截图上下文")
	}
	_, port, err := net.SplitHostPort(configuration.Address)
	parsed, parseErr := strconv.ParseUint(port, 10, 16)
	if err != nil || parseErr != nil || parsed == 0 {
		return errors.New("服务监听地址无效")
	}
	origin, err := url.Parse(configuration.AdminOrigin)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return errors.New("管理平台来源必须是明确的 HTTPS 来源")
	}
	nodeURL, err := url.Parse(configuration.Node.URL)
	if err != nil || nodeURL.Scheme != "https" || nodeURL.Host == "" || nodeURL.User != nil || nodeURL.Path != "" || nodeURL.RawQuery != "" || nodeURL.Fragment != "" {
		return errors.New("节点来源必须是明确的 HTTPS 来源")
	}
	if _, err := nodeapi.ParseFingerprints(configuration.Node.ServerFingerprints); err != nil {
		return err
	}
	paths := []string{configuration.AdminDirectory, configuration.TLSCertificateFile, configuration.TLSPrivateKeyFile, configuration.IdentityKeyFile, configuration.CSRFKeyFile, configuration.Node.CertificateFile, configuration.Node.PrivateKeyFile}
	if configuration.Models.RootCAFile != "" {
		paths = append(paths, configuration.Models.RootCAFile)
	}
	if len(configuration.Database.DSNFiles) != 4 || len(configuration.ModelKeyFiles) < 1 || len(configuration.ModelKeyFiles) > 16 || configuration.ModelKeyFiles[configuration.ActiveModelKey] == "" {
		return errors.New("数据库用途或模型加密文件配置缺失")
	}
	for _, role := range []postgres.Role{postgres.IdentityRole, postgres.ControlRole, postgres.TenantRole, postgres.DispatchRole} {
		paths = append(paths, configuration.Database.DSNFiles[role])
	}
	for _, path := range configuration.ModelKeyFiles {
		paths = append(paths, path)
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return errors.New("服务资源必须使用规范绝对路径")
		}
	}
	for _, timeout := range []Duration{configuration.HTTP.ReadHeaderTimeout, configuration.HTTP.ReadTimeout, configuration.HTTP.WriteTimeout, configuration.HTTP.IdleTimeout, configuration.HTTP.RequestTimeout, configuration.HTTP.ShutdownTimeout} {
		if timeout.Value() < time.Second || timeout.Value() > 5*time.Minute {
			return errors.New("HTTP 时限无效")
		}
	}
	if configuration.HTTP.RequestTimeout > Duration(time.Minute) || configuration.HTTP.WriteTimeout < configuration.HTTP.RequestTimeout || configuration.Provision.RequestWait >= configuration.HTTP.RequestTimeout || configuration.Models.VerificationTimeout >= configuration.HTTP.RequestTimeout || configuration.HTTP.ShutdownTimeout < configuration.Runs.RecordTimeout || configuration.Provision.CallTimeout > configuration.Node.CallTimeout || configuration.Computer.NodeTimeout > configuration.Node.CallTimeout || configuration.Runs.AgentTimeout > configuration.Node.CallTimeout {
		return errors.New("服务调用和退出时限不匹配")
	}
	return nil
}
