package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const DefaultDeepSeekBaseURL = "https://api.deepseek.com"
const DefaultDeepSeekContextLimit = 8192
const DefaultDeepSeekOutputLimit = 4096

// Config 集中描述部署差异；机密只在进程内读取，不序列化。
type Config struct {
	Addr, DataDir, WorkspaceDir, BrowserArchive string
	TLSCertFile, TLSKeyFile                     string
	HealthToken                                 string
	ClockURL                                    string
	DeepSeekBaseURL                             string
	DeepSeekContextLimit, DeepSeekOutputLimit   int
	MaxConcurrentRuns                           int
	StepTimeout                                 time.Duration
	RunMaxSteps                                 int
	RunTimeout                                  time.Duration
	RunBudgetMicro                              int64
}

// Load 读取 TOM_ 环境变量；无效值返回错误，避免悄悄使用默认值。
func Load() (Config, error) {
	executable, err := os.Executable()
	if err != nil {
		return Config{}, err
	}
	base := filepath.Dir(executable)
	c := Config{Addr: ":8787", DataDir: filepath.Join(base, "data"), WorkspaceDir: filepath.Join(base, "workspace"), DeepSeekBaseURL: DefaultDeepSeekBaseURL, MaxConcurrentRuns: 3, StepTimeout: 10 * time.Minute, RunMaxSteps: 60, RunTimeout: 30 * time.Minute, RunBudgetMicro: 2_000_000}
	c.DeepSeekContextLimit, c.DeepSeekOutputLimit = DefaultDeepSeekContextLimit, DefaultDeepSeekOutputLimit
	for key, dst := range map[string]*string{"TOM_ADDR": &c.Addr, "TOM_TLS_CERT_FILE": &c.TLSCertFile, "TOM_TLS_KEY_FILE": &c.TLSKeyFile, "TOM_DATA_DIR": &c.DataDir, "TOM_WORKSPACE_DIR": &c.WorkspaceDir, "TOM_BROWSER_ARCHIVE": &c.BrowserArchive, "TOM_HEALTH_TOKEN": &c.HealthToken} {
		if value, ok := os.LookupEnv(key); ok {
			if value == "" {
				return c, fmt.Errorf("配置 %s 不能为空", key)
			}
			*dst = value
		}
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return c, fmt.Errorf("启用 HTTPS 必须同时配置 TOM_TLS_CERT_FILE 和 TOM_TLS_KEY_FILE")
	}
	if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		return c, fmt.Errorf("监听地址无效: %w", err)
	}
	if value, ok := os.LookupEnv("TOM_CLOCK_URL"); ok {
		address, err := url.Parse(value)
		if err != nil || address.Scheme != "https" || address.Hostname() == "" || address.User != nil || address.RawQuery != "" || address.ForceQuery || address.Fragment != "" {
			return c, fmt.Errorf("时钟校验地址必须为 HTTPS，且不能包含用户信息、查询参数或片段")
		}
		c.ClockURL = address.String()
	}
	if value, ok := os.LookupEnv("TOM_DEEPSEEK_BASE_URL"); ok {
		address, parseErr := url.Parse(value)
		if parseErr != nil || address.Hostname() == "" || address.User != nil || address.RawQuery != "" || address.ForceQuery || address.Fragment != "" || (address.Scheme != "https" && !(address.Scheme == "http" && (address.Hostname() == "localhost" || net.ParseIP(address.Hostname()).IsLoopback()))) {
			return c, fmt.Errorf("DeepSeek 地址必须为 HTTPS 或本机环回地址，且不能包含用户信息、查询参数或片段")
		}
		c.DeepSeekBaseURL = strings.TrimRight(address.String(), "/")
	}
	for key, destination := range map[string]*int{"TOM_DEEPSEEK_CONTEXT_LIMIT": &c.DeepSeekContextLimit, "TOM_DEEPSEEK_OUTPUT_LIMIT": &c.DeepSeekOutputLimit} {
		if value, ok := os.LookupEnv(key); ok {
			*destination, err = strconv.Atoi(value)
			if err != nil || *destination < 1 || *destination > 10000000 {
				return c, fmt.Errorf("配置 %s 必须为 1 到 10000000 的整数", key)
			}
		}
	}
	if c.DeepSeekContextLimit < 256 || c.DeepSeekOutputLimit > c.DeepSeekContextLimit {
		return c, fmt.Errorf("DeepSeek 应用上下文限额至少为 256，输出限额不能超过上下文限额")
	}
	if value, ok := os.LookupEnv("TOM_MAX_CONCURRENT_RUNS"); ok {
		c.MaxConcurrentRuns, err = strconv.Atoi(value)
		if err != nil || c.MaxConcurrentRuns < 1 || c.MaxConcurrentRuns > 64 {
			return c, fmt.Errorf("并发数必须在 1 到 64 之间")
		}
	}
	if value, ok := os.LookupEnv("TOM_RUN_MAX_STEPS"); ok {
		c.RunMaxSteps, err = strconv.Atoi(value)
		if err != nil || c.RunMaxSteps < 1 || c.RunMaxSteps > 1000 {
			return c, fmt.Errorf("执行步数必须在 1 到 1000 之间")
		}
	}
	if value, ok := os.LookupEnv("TOM_RUN_TIMEOUT"); ok {
		c.RunTimeout, err = time.ParseDuration(value)
		if err != nil || c.RunTimeout < time.Second || c.RunTimeout > 24*time.Hour {
			return c, fmt.Errorf("执行时限必须在 1 秒到 24 小时之间")
		}
	}
	if value, ok := os.LookupEnv("TOM_RUN_BUDGET_MICRO"); ok {
		c.RunBudgetMicro, err = strconv.ParseInt(value, 10, 64)
		if err != nil || c.RunBudgetMicro < 1 || c.RunBudgetMicro > 1_000_000_000 {
			return c, fmt.Errorf("执行费用上限必须在 1 到 1000000000 微美元之间")
		}
	}
	c.DataDir, err = filepath.Abs(c.DataDir)
	if err != nil {
		return c, err
	}
	c.WorkspaceDir, err = filepath.Abs(c.WorkspaceDir)
	if err != nil {
		return c, err
	}
	if c.DataDir == c.WorkspaceDir {
		return c, fmt.Errorf("数据目录与工作区不能相同")
	}
	return c, nil
}
