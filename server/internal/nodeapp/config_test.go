package nodeapp

import (
	"bytes"
	"encoding/json"
	"math"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/computeragent"
)

func testConfig() Config {
	return Config{
		Version: 2, NodeID: "01K9AX6VSMKDBZ049G41FT4WHW", Listen: "10.0.0.5:9443", AllowPublic: false, Control: ControlTLS{CertificateFile: "/etc/tombot/tls/node-certificate.pem", PrivateKeyFile: "/etc/tombot/private/node.key", ClientFingerprints: []string{strings.Repeat("a", 64)}}, StartupTimeout: 120000, ShutdownTimeout: 180000,
		API:          APIConfig{CallTimeout: 30000, ShutdownTimeout: 90000, MaximumConnections: 32},
		Runtime:      RuntimeConfig{Socket: "/run/docker.sock", StateDirectory: "/var/lib/tombot-node/runtime", StorageDirectory: "/var/lib/tombot-node/workspaces", CallTimeout: 10000, MaximumLease: 120000, TemporaryBytes: 64 << 20, RuntimeBytes: 128 << 20, SharedMemoryBytes: 256 << 20, MaximumEgressComputers: 8},
		Storage:      StorageConfig{StateDirectory: "/var/lib/tombot-node/storage", CommandTimeout: 10000, MinimumFreeBytes: 1 << 30},
		Groups:       GroupConfig{GracePeriod: 20, PollInterval: 5, MaximumProcesses: 64},
		Sessions:     SessionConfig{MaximumActive: 8, CallTimeout: 10000, PollInterval: 10, RetryDelay: 100, CleanupAttempts: 2},
		Browser:      BrowserConfig{CallTimeout: 1000, CleanupTimeout: 10000, ShutdownTimeout: 10000, Limits: computeragent.BrowserLimits{Width: 1280, Height: 800, MaximumWindows: 32, MaximumPerSession: 4, MaximumWindowRecords: 128, MaximumEvents: 256, CallTimeoutMS: 1000, DialogTimeoutMS: 10000, ShutdownTimeoutMS: 1000}},
		Probe:        ProbeConfig{CallTimeout: 10000, CleanupTimeout: 10000},
		AgentStartup: AgentStartupConfig{Timeout: 1000, CallTimeout: 1000, RetryDelay: 10, MaximumAttempts: 4},
		Agent:        computeragent.RuntimeOptions{CallTimeoutMS: 1000, CleanupTimeoutMS: 1000, RetryDelayMS: 100, CleanupAttempts: 2, PollIntervalMS: 10, ShutdownTimeoutMS: 5000, MaximumActive: 8, MaximumConnections: 16, EgressConnectTimeoutMS: 1000, EgressIdleTimeoutMS: 1000, EgressLifetimeMS: 10000, EgressMaximumConnections: 8, EgressMaximumTransferBytes: 1 << 20},
		Egress:       EgressConfig{Ports: []uint16{80, 443}, DeniedNetworks: []netip.Prefix{}, ConnectTimeout: 1000, HeaderTimeout: 1000, IdleTimeout: 1000, RequestTimeout: 10000, TunnelTimeout: 10000, ShutdownTimeout: 2000, MaximumConnections: 8, MaximumRequestBytes: 1 << 20, MaximumResponseBytes: 1 << 20, MaximumTunnelBytes: 1 << 20},
	}
}

func TestConfigValidation(t *testing.T) {
	valid := testConfig()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Config){
		"agent_startup_missing":    func(c *Config) { c.AgentStartup = AgentStartupConfig{} },
		"agent_startup_attempts":   func(c *Config) { c.AgentStartup.MaximumAttempts = 17 },
		"agent_startup_delay":      func(c *Config) { c.AgentStartup.RetryDelay = 0 },
		"agent_startup_call":       func(c *Config) { c.AgentStartup.CallTimeout = 2000 },
		"agent_startup_overflow":   func(c *Config) { c.AgentStartup.Timeout = Milliseconds(math.MaxInt64) },
		"agent_startup_api_budget": func(c *Config) { c.AgentStartup.Timeout = 20000 },
		"browser_missing":          func(c *Config) { c.Browser = BrowserConfig{} },
		"browser_agent_budget":     func(c *Config) { c.Browser.Limits.CallTimeoutMS = 2000 },
		"browser_agent_cleanup":    func(c *Config) { c.Browser.Limits.ShutdownTimeoutMS = 2000 },
		"browser_cleanup":          func(c *Config) { c.Browser.CleanupTimeout = 2000 },
		"browser_shutdown":         func(c *Config) { c.Browser.ShutdownTimeout = 1000 },
		"browser_api_budget": func(c *Config) {
			c.Browser.CallTimeout = 40000
			c.Browser.CleanupTimeout = 60000
			c.Browser.ShutdownTimeout = 60000
		},
		"version": func(c *Config) { c.Version = 1 }, "node": func(c *Config) { c.NodeID = "invalid" }, "fingerprint_empty": func(c *Config) { c.Control.ClientFingerprints = nil }, "fingerprint_format": func(c *Config) { c.Control.ClientFingerprints = []string{"ABCDEF"} }, "certificate_path": func(c *Config) { c.Control.CertificateFile = "relative.pem" },
		"startup_budget": func(c *Config) { c.StartupTimeout = 1000 }, "shutdown_budget": func(c *Config) { c.ShutdownTimeout = 1000 }, "overflow": func(c *Config) { c.StartupTimeout = Milliseconds(math.MaxInt64) },
		"api_budget": func(c *Config) { c.API.CallTimeout = 1000 }, "agent_budget": func(c *Config) { c.Agent.CallTimeoutMS = 11000; c.Agent.ShutdownTimeoutMS = 15000 }, "cleanup_budget": func(c *Config) { c.Probe.CleanupTimeout = 60000; c.API.ShutdownTimeout = 30000 },
		"listen": func(c *Config) { c.Listen = "relative" }, "listen_port": func(c *Config) { c.Listen = "10.0.0.5:0" }, "listen_newline": func(c *Config) { c.Listen += "\n" }, "docker_newline": func(c *Config) { c.Runtime.Socket += "\n" },
		"private_certificate": func(c *Config) { c.Control.PrivateKeyFile = c.Storage.StateDirectory + "/node.key" }, "root_directory": func(c *Config) { c.Runtime.StorageDirectory = "/" }, "overlapping": func(c *Config) { c.Storage.StateDirectory = c.Runtime.StateDirectory + "/storage" }, "ancestor": func(c *Config) { c.Storage.StateDirectory = "/var/lib" },
		"agent": func(c *Config) { c.Agent.MaximumActive = 0 }, "egress": func(c *Config) { c.Egress.MaximumConnections = 0 }, "storage": func(c *Config) { c.Storage.MinimumFreeBytes = 0 }, "groups": func(c *Config) { c.Groups.MaximumProcesses = 0 }, "sessions": func(c *Config) { c.Sessions.MaximumActive = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			c := testConfig()
			change(&c)
			if c.Validate() == nil {
				t.Fatal("接受了无效节点配置")
			}
		})
	}
	for _, value := range []Milliseconds{-1, 0, Milliseconds(math.MaxInt64), 3600001} {
		if value.duration() != 0 {
			t.Fatal("非法时限没有拒绝", value)
		}
	}
	if Milliseconds(1000).duration() != time.Second {
		t.Fatal("时限转换错误")
	}
}

func TestStrictNestedConfig(t *testing.T) {
	encoded, err := json.MarshalIndent(testConfig(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var result Config
	if err := decodeConfig(encoded, &result); err != nil || result.Validate() != nil {
		t.Fatal("拒绝规范配置", err)
	}
	for name, input := range map[string][]byte{
		"duplicate": []byte(`{"version":1,"version":1}`), "alias": []byte(`{"Version":1}`), "nested_alias": []byte(`{"api":{"CALL_TIMEOUT_MS":1000}}`), "nested_duplicate": []byte(`{"api":{"call_timeout_ms":1000,"call_timeout_ms":2000}}`),
		"null": []byte(`{"egress":{"denied_networks":null}}`), "unknown": []byte(`{"agent":{"unchecked":true}}`), "array": []byte(`[]`), "trailing": []byte(`{} {}`), "overflow": []byte(`{"startup_timeout_ms":9223372036854775808}`), "fraction": []byte(`{"version":1.0}`), "unicode": []byte{'{', '"', 255, '"', ':', '1', '}'}, "too_large": bytes.Repeat([]byte(" "), (16<<10)+1), "depth": []byte(`{"a":[[[[[[[[[1]]]]]]]]]}`),
	} {
		t.Run(name, func(t *testing.T) {
			var c Config
			if decodeConfig(input, &c) == nil {
				t.Fatal("歧义配置通过")
			}
		})
	}
	for _, input := range []string{` { "listen":"10.0.0.5:9443", "version":1 } `, `{"egress":{"ports":[80,443],"denied_networks":[]}}`} {
		var c Config
		if decodeConfig([]byte(input), &c) != nil {
			t.Fatal("正常重排或空列表被拒绝")
		}
	}
}

func TestConfigEntryRequiresLinuxRoot(t *testing.T) {
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		return
	}
	if _, err := LoadConfig("/no-such-node.json"); err == nil || !strings.Contains(err.Error(), "Linux root") {
		t.Fatal(err)
	}
	if _, err := New(t.Context(), testConfig(), testLogger()); err == nil {
		t.Fatal("非 Linux root 创建了宿主节点")
	}
}

func TestDeploymentExample(t *testing.T) {
	encoded, err := os.ReadFile("../../deploy/node.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var configuration Config
	if err := decodeConfig(encoded, &configuration); err != nil {
		t.Fatal(err)
	}
	if err := configuration.Validate(); err != nil {
		t.Fatal("部署示例与正式配置契约不符", err)
	}
}
