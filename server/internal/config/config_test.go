package config

import "testing"

func TestEnvironmentValidation(t *testing.T) {
	for _, value := range []string{"0", "65", "invalid", ""} {
		t.Run("并发配置="+value, func(t *testing.T) {
			t.Setenv("TOM_MAX_CONCURRENT_RUNS", value)
			if _, err := Load(); err == nil {
				t.Fatal("无效并发被接受")
			}
		})
	}
	t.Setenv("TOM_ADDR", "127.0.0.1:8787")
	t.Setenv("TOM_MAX_CONCURRENT_RUNS", "3")
	t.Setenv("TOM_DATA_DIR", t.TempDir())
	t.Setenv("TOM_WORKSPACE_DIR", t.TempDir())
	cfg, err := Load()
	if err != nil || cfg.MaxConcurrentRuns != 3 {
		t.Fatalf("配置读取失败: %v", err)
	}
}

func TestClockSourceRejectsCredentialsAndPlaintext(t *testing.T) {
	for _, address := range []string{"", "http://clock.example", "https://name@clock.example", "https://clock.example/path?q=value", "https://clock.example/#fragment"} {
		t.Setenv("TOM_CLOCK_URL", address)
		if _, err := Load(); err == nil {
			t.Fatal("无效时间参照被接受")
		}
	}
	const address = "https://clock.example/health"
	t.Setenv("TOM_CLOCK_URL", address)
	cfg, err := Load()
	if err != nil || cfg.ClockURL != address {
		t.Fatal("未使用指定参照", err)
	}
}

func TestTLSConfigurationRequiresBothFiles(t *testing.T) {
	t.Setenv("TOM_TLS_CERT_FILE", "certificate.pem")
	if _, err := Load(); err == nil {
		t.Fatal("接受仅有证书的配置")
	}
	t.Setenv("TOM_TLS_KEY_FILE", "private.pem")
	cfg, err := Load()
	if err != nil || cfg.TLSCertFile != "certificate.pem" || cfg.TLSKeyFile != "private.pem" {
		t.Fatal("未正确加载 TLS 路径", err)
	}
	t.Setenv("TOM_TLS_CERT_FILE", "")
	if _, err = Load(); err == nil {
		t.Fatal("接受空证书路径")
	}
}

func TestDeepSeekDeploymentConfiguration(t *testing.T) {
	for _, address := range []string{"", "http://public.example", "https://name@example.com", "https://example.com/?", "https://example.com/?credential=hidden", "https://example.com/#fragment"} {
		t.Run(address, func(t *testing.T) {
			t.Setenv("TOM_DEEPSEEK_BASE_URL", address)
			if _, err := Load(); err == nil {
				t.Fatal("不安全供应商覆盖地址被接受")
			}
		})
	}
	t.Setenv("TOM_DEEPSEEK_BASE_URL", "http://127.0.0.1:1234/v1/")
	t.Setenv("TOM_DEEPSEEK_CONTEXT_LIMIT", "16384")
	t.Setenv("TOM_DEEPSEEK_OUTPUT_LIMIT", "2048")
	cfg, err := Load()
	if err != nil || cfg.DeepSeekBaseURL != "http://127.0.0.1:1234/v1" || cfg.DeepSeekContextLimit != 16384 || cfg.DeepSeekOutputLimit != 2048 {
		t.Fatal("供应商配置未生效", err)
	}
	for _, limit := range []string{"0", "invalid", "20000"} {
		t.Setenv("TOM_DEEPSEEK_OUTPUT_LIMIT", limit)
		if _, err = Load(); err == nil {
			t.Fatal("非法应用输出限额被接受")
		}
	}
}
