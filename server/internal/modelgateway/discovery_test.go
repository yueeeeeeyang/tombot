package modelgateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/testkit"
)

func TestDiscoverModelsWithoutAllowlist(t *testing.T) {
	for _, protocol := range []string{"openai_compat", "responses", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			credential := testkit.Random(t)
			fixture := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/v1/models" {
					t.Error("模型发现端点错误")
				}
				if protocol == "anthropic" {
					if r.Header.Get("x-api-key") != credential {
						t.Error("模型发现认证缺失")
					}
				} else if r.Header.Get("Authorization") != "Bearer "+credential {
					t.Error("模型发现认证缺失")
				}
				w.Header().Set("Content-Type", "application/json")
				if _, err := fmt.Fprint(w, `{"object":"list","data":[{"id":"model-a","display_name":"模型甲"},{"id":"model-a"},{"id":"model-b"}]}`); err != nil {
					t.Error(err)
				}
			}, nil)
			fixture.resolver.set("198.18.0.50")
			models, err := fixture.gateway.Discover(context.Background(), protocol, fixture.configuration.BaseURL, []byte(credential))
			if err != nil || len(models) != 2 || models[0].DisplayName != "模型甲" || models[1].ModelKey != "model-b" {
				t.Fatal("代理环境模型发现失败", err)
			}
		})
	}
}

func TestDiscoverModelsFailures(t *testing.T) {
	for _, test := range []struct {
		status  int
		failure error
	}{{401, ErrDiscoveryAuthentication}, {403, ErrDiscoveryAuthentication}, {404, ErrDiscoveryUnsupported}, {405, ErrDiscoveryUnsupported}, {429, ErrDiscoveryRateLimited}, {503, ErrUnavailable}, {307, ErrUnavailable}} {
		t.Run(fmt.Sprint(test.status), func(t *testing.T) {
			calls := 0
			fixture := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "https://other.example.test")
				w.WriteHeader(test.status)
			}, nil)
			_, err := fixture.gateway.Discover(context.Background(), "responses", fixture.configuration.BaseURL, []byte(testkit.Random(t)))
			if !errors.Is(err, test.failure) || calls != 1 {
				t.Fatal("状态未分类或发生跳转、重试", err)
			}
		})
	}
	for _, body := range []string{`{}`, `{"object":"list","data":null}`, `{"object":"list","data":[{"id":""}]}`, `{"object":"list","data":[]} {}`, `{"object":"list","data":[{"id":"bad\nkey"}]}`, strings.Repeat("x", maxDiscoveryBytes+1)} {
		fixture := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := fmt.Fprint(w, body); err != nil {
				t.Error(err)
			}
		}, nil)
		if _, err := fixture.gateway.Discover(context.Background(), "responses", fixture.configuration.BaseURL, []byte(testkit.Random(t))); err == nil {
			t.Fatal("无效响应被接受")
		}
	}
	if models, err := parseDiscovered("responses", []byte(`{"object":"list","data":[]}`)); err != nil || models == nil || len(models) != 0 {
		t.Fatal("空模型列表未按空结果返回", err)
	}
	fixture := newFixture(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, func(options *Options) {
		options.RequestTimeout = 50 * time.Millisecond
		options.ConnectTimeout = time.Millisecond * 20
		options.TLSHandshakeTimeout = time.Millisecond * 20
		options.ResponseHeaderTimeout = time.Millisecond * 50
		options.IdleReadTimeout = time.Millisecond * 50
		options.WriteTimeout = time.Millisecond * 50
	})
	_, err := fixture.gateway.Discover(context.Background(), "responses", fixture.configuration.BaseURL, []byte(testkit.Random(t)))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("模型发现超时未传播", err)
	}
}

func TestDiscoveredModelMetadata(t *testing.T) {
	models, err := parseDiscovered("responses", []byte(`{"object":"list","data":[{"id":"image-model","name":"图像模型","context_window":1048576,"max_output_tokens":393216,"input_modalities":["text","image"],"output_modalities":["text"],"effort":{"supported_levels":["low","high","max"],"default_level":"high"},"supports_tools":true},{"id":"text-model","context_window":32768,"max_output_tokens":4096,"input_modalities":["text"],"effort":{"supported_levels":["low","high"],"default_level":"low"}},{"id":"unknown-model"}]}`))
	if err != nil || len(models) != 3 {
		t.Fatal("模型元数据读取失败", err)
	}
	first, second, missing := models[0], models[1], models[2]
	if first.DisplayName != "图像模型" || *first.ContextLimit != 1048576 || *first.OutputLimit != 393216 || first.SupportsVision == nil || !*first.SupportsVision || first.SupportsTools == nil || !*first.SupportsTools || first.Metadata.Effort.DefaultLevel != "high" {
		t.Fatal("供应商规格被丢弃")
	}
	if *second.ContextLimit != 32768 || *second.OutputLimit != 4096 || second.SupportsVision == nil || *second.SupportsVision || second.SupportsTools != nil || second.Metadata.Effort.DefaultLevel != "low" {
		t.Fatal("不同模型套用了相同规格")
	}
	if missing.ContextLimit != nil || missing.OutputLimit != nil || missing.SupportsVision != nil || missing.SupportsTools != nil || missing.Metadata.InputModalities != nil || missing.Metadata.Effort != nil {
		t.Fatal("缺失元数据被填入虚假默认值")
	}
	anthropic, err := parseDiscovered("anthropic", []byte(`{"data":[{"id":"claude-model","display_name":"模型名称","max_input_tokens":200000,"max_tokens":64000,"capabilities":{"image_input":{"supported":true},"effort":{"supported":true,"low":{"supported":true},"high":{"supported":true},"max":{"supported":false}}}}],"has_more":false}`))
	if err != nil || len(anthropic) != 1 || *anthropic[0].ContextLimit != 200000 || *anthropic[0].OutputLimit != 64000 || !*anthropic[0].SupportsVision || len(anthropic[0].Metadata.Effort.SupportedLevels) != 2 || anthropic[0].Metadata.Effort.DefaultLevel != "" {
		t.Fatal("Anthropic 元数据规范化失败", err)
	}
	for _, item := range []string{
		`"context_window":0`, `"context_window":100000001`, `"context_window":"8192"`,
		`"context_window":100,"max_output_tokens":200`,
		`"input_modalities":["text","text"]`, `"input_modalities":["bad\nmedia"]`,
		`"input_modalities":["text"],"supports_vision":true`,
		`"effort":{"supported_levels":["low"],"default_level":"high"}`,
		`"effort":{"supported_levels":[]}`, `"supports_tools":"yes"`,
	} {
		if _, err := parseDiscovered("responses", []byte(`{"object":"list","data":[{"id":"invalid",`+item+`}]}`)); !errors.Is(err, ErrProtocol) {
			t.Fatal("非法规格未拒绝", item, err)
		}
	}
}
