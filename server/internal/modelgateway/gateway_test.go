package modelgateway

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
)

type testResolver struct {
	mu        sync.Mutex
	addresses []netip.Addr
	calls     int
}

func (resolver *testResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	resolver.calls++
	return append([]netip.Addr(nil), resolver.addresses...), nil
}
func (resolver *testResolver) set(addresses ...string) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	resolver.addresses = nil
	for _, address := range addresses {
		resolver.addresses = append(resolver.addresses, netip.MustParseAddr(address))
	}
}

type testDialer struct {
	destination string
	calls       atomic.Int64
	mu          sync.Mutex
	addresses   []string
}

func (dialer *testDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	dialer.calls.Add(1)
	dialer.mu.Lock()
	dialer.addresses = append(dialer.addresses, address)
	dialer.mu.Unlock()
	if _, err := netip.ParseAddrPort(address); err != nil {
		return nil, errors.New("连接器收到未解析的域名")
	}
	return (&net.Dialer{}).DialContext(ctx, network, dialer.destination)
}

type fixture struct {
	gateway       *Gateway
	configuration modelregistry.Configuration
	resolver      *testResolver
	dialer        *testDialer
}

func newFixture(t *testing.T, handler http.HandlerFunc, change func(*Options)) fixture {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	root := x509.NewCertPool()
	root.AddCert(server.Certificate())
	origin := "https://example.com:" + address.Port()
	options := Options{Origins: []Origin{{URL: origin}}, RootCAs: root, ConnectTimeout: time.Second, TLSHandshakeTimeout: time.Second, ResponseHeaderTimeout: time.Second, IdleReadTimeout: time.Second, WriteTimeout: time.Second, RequestTimeout: 3 * time.Second, MaxRequestBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxConcurrentRequests: 2}
	if change != nil {
		change(&options)
	}
	gateway, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &testResolver{}
	resolver.set("93.184.216.34")
	dialer := &testDialer{destination: address.Host}
	gateway.resolver = resolver
	gateway.dialer = dialer
	return fixture{gateway, modelregistry.Configuration{Protocol: "openai_compat", BaseURL: origin + "/v1", ModelKey: "fixture-model", ContextLimit: 4096, OutputLimit: 256}, resolver, dialer}
}
func basicInput() Input {
	return Input{Messages: []provider.Message{{Role: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: "Hello"}}}}}
}
func frames(t *testing.T, w http.ResponseWriter, values ...string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	for _, value := range values {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", value); err != nil {
			t.Error("写入测试流失败")
			return
		}
	}
	w.(http.Flusher).Flush()
}
func textFrames(t *testing.T, w http.ResponseWriter, text string, usage bool) {
	values := []string{fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%q},"finish_reason":"stop"}]}`, text)}
	if usage {
		values = append(values, `{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":3}}}`)
	}
	frames(t, w, append(values, "[DONE]")...)
}

func TestGatewayTLSStreamingAndUsage(t *testing.T) {
	for _, reported := range []bool{true, false} {
		t.Run(fmt.Sprint(reported), func(t *testing.T) {
			fixture := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" || r.Method != "POST" {
					t.Error("模型路径错误")
				}
				var request struct {
					Model string `json:"model"`
					Max   int    `json:"max_completion_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request.Model != "fixture-model" || request.Max != 256 {
					t.Error("请求未使用服务端固定模型")
				}
				textFrames(t, w, "已回答", reported)
			}, nil)
			var text strings.Builder
			result, err := fixture.gateway.Call(context.Background(), fixture.configuration, nil, basicInput(), func(event provider.Event) error {
				if event.Kind == "text_delta" {
					text.WriteString(event.Text)
				}
				return nil
			})
			if err != nil || !result.IsComplete || text.String() != "已回答" {
				t.Fatal("TLS 模型流失败", err)
			}
			if reported {
				if result.Usage == nil || result.Usage.InputTokens == nil || *result.Usage.InputTokens != 12 || result.Usage.OutputTokens == nil || *result.Usage.OutputTokens != 4 || !result.Usage.InputIncludesCache {
					t.Fatal("真实用量缺失或被改写")
				}
			} else if result.Usage != nil {
				t.Fatal("未知用量被估算或补零")
			}
			if fixture.dialer.calls.Load() != 1 {
				t.Fatal("一次调用建立了重复请求")
			}
		})
	}
}

func TestGatewayAddressBoundaries(t *testing.T) {
	fixture := newFixture(t, func(w http.ResponseWriter, r *http.Request) { textFrames(t, w, "ok", true) }, nil)
	for _, addresses := range [][]string{{"127.0.0.1"}, {"10.0.0.2"}, {"169.254.169.254"}, {"fd00:ec2::254"}, {"168.63.129.16"}, {"::ffff:127.0.0.1"}, {"::1"}, {"fe80::1"}, {"100.64.0.1"}, {"192.0.2.1"}, {"93.184.216.34", "10.0.0.1"}} {
		fixture.resolver.set(addresses...)
		_, err := fixture.gateway.Call(context.Background(), fixture.configuration, nil, basicInput(), func(provider.Event) error { return nil })
		if !errors.Is(err, ErrAddressDenied) {
			t.Fatalf("解析地址未拒绝：%v %v", addresses, err)
		}
	}
	if fixture.dialer.calls.Load() != 0 {
		t.Fatal("拒绝地址仍被连接")
	}
	fixture.resolver.set("93.184.216.34")
	if _, err := fixture.gateway.Call(context.Background(), fixture.configuration, nil, basicInput(), func(provider.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	fixture.resolver.set("10.0.0.1")
	_, err := fixture.gateway.Call(context.Background(), fixture.configuration, nil, basicInput(), func(provider.Event) error { return nil })
	if !errors.Is(err, ErrAddressDenied) || fixture.dialer.calls.Load() != 1 {
		t.Fatal("新调用复用了旧 DNS 决策", err)
	}
	fixture.configuration.BaseURL = "https://unapproved.example.test/v1"
	_, err = fixture.gateway.Call(context.Background(), fixture.configuration, nil, basicInput(), func(provider.Event) error { return nil })
	if !errors.Is(err, ErrAddressDenied) {
		t.Fatal("未授权来源未拒绝", err)
	}
	private := newFixture(t, func(w http.ResponseWriter, r *http.Request) { textFrames(t, w, "ok", true) }, func(options *Options) {
		options.Origins[0].Networks = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}
		options.DeniedNetworks = []netip.Prefix{netip.MustParsePrefix("10.0.0.1/32")}
	})
	private.resolver.set("10.0.0.2")
	if _, err = private.gateway.Call(context.Background(), private.configuration, nil, basicInput(), func(provider.Event) error { return nil }); err != nil {
		t.Fatal("显式内网模型被拒绝", err)
	}
	private.resolver.set("10.0.0.1")
	_, err = private.gateway.Call(context.Background(), private.configuration, nil, basicInput(), func(provider.Event) error { return nil })
	if !errors.Is(err, ErrAddressDenied) {
		t.Fatal("拒绝范围未覆盖允许范围", err)
	}
}

func TestGatewayOriginHostCase(t *testing.T) {
	for _, uppercaseRule := range []bool{true, false} {
		t.Run(fmt.Sprint(uppercaseRule), func(t *testing.T) {
			fixture := newFixture(t, func(w http.ResponseWriter, r *http.Request) { textFrames(t, w, "ok", true) }, func(options *Options) {
				if uppercaseRule {
					options.Origins[0].URL = strings.Replace(options.Origins[0].URL, "example.com", "EXAMPLE.COM", 1)
				}
			})
			if !uppercaseRule {
				fixture.configuration.BaseURL = strings.Replace(fixture.configuration.BaseURL, "example.com", "EXAMPLE.COM", 1)
			}
			result, err := fixture.gateway.Call(context.Background(), fixture.configuration, nil, basicInput(), func(provider.Event) error { return nil })
			if err != nil || !result.IsComplete || fixture.dialer.calls.Load() != 1 {
				t.Fatal("相同来源因域名大小写而被拒绝", err)
			}
		})
	}
}

func TestGatewayRejectsTranslationEvenInsideAllowedNetwork(t *testing.T) {
	fixture := newFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("转换地址不应到达供应商") }, func(options *Options) {
		options.Origins[0].Networks = []netip.Prefix{netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("2000::/3")}
	})
	for _, address := range []string{"64:ff9b::a9fe:a9fe", "64:ff9b:1::a9fe:a9fe", "2002:7f00:1::1", "2001::1"} {
		fixture.resolver.set(address)
		_, err := fixture.gateway.Call(context.Background(), fixture.configuration, nil, basicInput(), func(provider.Event) error { return nil })
		if !errors.Is(err, ErrAddressDenied) {
			t.Fatal("地址转换绕过部署限制", address, err)
		}
	}
	if fixture.dialer.calls.Load() != 0 {
		t.Fatal("转换地址仍被连接")
	}
}

func TestGatewayNoReplayRedirectProxyOrTLSBypass(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	for _, status := range []int{307, 401, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int64
			fixture := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "https://unapproved.example.test")
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(status)
			}, nil)
			_, err := fixture.gateway.Call(context.Background(), fixture.configuration, nil, basicInput(), func(provider.Event) error { return nil })
			if err == nil || calls.Load() != 1 || fixture.dialer.calls.Load() != 1 {
				t.Fatal("请求被重放、重定向或环境代理拦截", err)
			}
		})
	}
	fixture := newFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("证书未信任仍发送了 HTTP 请求") }, func(options *Options) { options.RootCAs = x509.NewCertPool() })
	if _, err := fixture.gateway.Call(context.Background(), fixture.configuration, nil, basicInput(), func(provider.Event) error { return nil }); err == nil {
		t.Fatal("TLS 验证被跳过")
	}
}

func TestGatewayLimitsCancellationAndQueue(t *testing.T) {
	t.Run("请求和响应大小", func(t *testing.T) {
		fixture := newFixture(t, func(w http.ResponseWriter, r *http.Request) { textFrames(t, w, strings.Repeat("x", 2048), true) }, func(options *Options) { options.MaxRequestBytes = 1024; options.MaxResponseBytes = 1024 })
		input := basicInput()
		input.System = strings.Repeat("a", 2048)
		_, err := fixture.gateway.Call(context.Background(), fixture.configuration, nil, input, func(provider.Event) error { return nil })
		if !errors.Is(err, ErrRequestTooLarge) || fixture.dialer.calls.Load() != 0 {
			t.Fatal("超大请求未提前拒绝", err)
		}
		_, err = fixture.gateway.Call(context.Background(), fixture.configuration, nil, basicInput(), func(provider.Event) error { return nil })
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatal("超大响应未拒绝", err)
		}
	})
	for _, body := range []bool{false, true} {
		t.Run(fmt.Sprint("超时", body), func(t *testing.T) {
			fixture := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error("测试请求体读取失败")
					return
				}
				if body {
					frames(t, w, `{"choices":[{"index":0,"delta":{"content":"partial"}}]}`)
				}
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
					t.Error("测试服务未观察到调用结束")
				}
			}, func(options *Options) {
				if body {
					options.IdleReadTimeout = 60 * time.Millisecond
				} else {
					options.ResponseHeaderTimeout = 60 * time.Millisecond
				}
			})
			started := time.Now()
			result, err := fixture.gateway.Call(context.Background(), fixture.configuration, nil, basicInput(), func(provider.Event) error { return nil })
			if err == nil || result.IsComplete || time.Since(started) > 800*time.Millisecond {
				t.Fatal("响应头或流空闲超时未生效", err)
			}
		})
	}
	t.Run("调用槽位和取消", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		fixture := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				t.Error("测试请求体读取失败")
				return
			}
			close(started)
			select {
			case <-release:
				textFrames(t, w, "ok", true)
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
				t.Error("测试调用未收到释放或取消")
			}
		}, func(options *Options) { options.MaxConcurrentRequests = 1 })
		finished := make(chan error, 1)
		go func() {
			_, err := fixture.gateway.Call(context.Background(), fixture.configuration, nil, basicInput(), func(provider.Event) error { return nil })
			finished <- err
		}()
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("首个调用未开始")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, err := fixture.gateway.Call(ctx, fixture.configuration, nil, basicInput(), func(provider.Event) error { return nil })
		if !errors.Is(err, context.DeadlineExceeded) || fixture.dialer.calls.Load() != 1 {
			t.Fatal("排队取消没有阻止请求", err)
		}
		close(release)
		if err = <-finished; err != nil {
			t.Fatal(err)
		}
	})
	t.Run("回调错误不重试", func(t *testing.T) {
		fixture := newFixture(t, func(w http.ResponseWriter, r *http.Request) { textFrames(t, w, "ok", true) }, nil)
		failure := errors.New("执行器已停止接收")
		result, err := fixture.gateway.Call(context.Background(), fixture.configuration, nil, basicInput(), func(provider.Event) error { return failure })
		if !errors.Is(err, failure) || result.IsComplete || fixture.dialer.calls.Load() != 1 {
			t.Fatal("回调中止未传播", err)
		}
	})
}

func TestGatewayRejectsResponseProtocol(t *testing.T) {
	for _, contentType := range []string{"application/json", "text/plain"} {
		fixture := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", contentType)
			w.WriteHeader(200)
		}, nil)
		if _, err := fixture.gateway.Call(context.Background(), fixture.configuration, nil, basicInput(), func(provider.Event) error { return nil }); !errors.Is(err, ErrProtocol) {
			t.Fatal("错误响应类型被接受", err)
		}
	}
}

func readProbe(t *testing.T, r *http.Request) (string, string, bool) {
	t.Helper()
	var request struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 1 {
		t.Fatal("探测消息数量错误")
	}
	var prompt, pixels string
	if request.Messages[0].Content[0] == '"' {
		if err := json.Unmarshal(request.Messages[0].Content, &prompt); err != nil {
			t.Fatal(err)
		}
	} else {
		var parts []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
			Source struct {
				Data string `json:"data"`
			} `json:"source"`
		}
		if err := json.Unmarshal(request.Messages[0].Content, &parts); err != nil {
			t.Fatal(err)
		}
		for _, part := range parts {
			if part.Type == "text" && prompt == "" {
				prompt = part.Text
			}
			if part.Type == "image_url" {
				pixels = strings.TrimPrefix(part.ImageURL.URL, "data:image/png;base64,")
			}
			if part.Type == "image" {
				pixels = part.Source.Data
			}
		}
	}
	return prompt, pixels, len(request.Tools) > 0
}
func probeReply(t *testing.T, prompt, pixels string, tool bool) string {
	t.Helper()
	if pixels != "" {
		encoded, err := base64.StdEncoding.DecodeString(pixels)
		if err != nil {
			t.Fatal(err)
		}
		image, err := png.Decode(bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		names := []string{}
		for index := range 6 {
			red, green, blue, _ := image.At(index*100+50, 100).RGBA()
			name := map[[3]uint32]string{{65535, 0, 0}: "red", {0, 65535, 0}: "green", {0, 0, 65535}: "blue", {65535, 65535, 0}: "yellow", {0, 65535, 65535}: "cyan", {65535, 0, 65535}: "magenta"}[[3]uint32{red, green, blue}]
			if name == "" {
				t.Fatal("视觉探测像素错误")
			}
			names = append(names, name)
		}
		return strings.Join(names, ",")
	}
	if tool {
		value := strings.Split(prompt, "nonce ")
		if len(value) != 2 {
			t.Fatal("工具探测未包含挑战")
		}
		return strings.SplitN(value[1], ".", 2)[0]
	}
	return strings.TrimPrefix(prompt, "Return exactly this verification value, with no other text: ")
}

func TestGatewayVerifiesCapabilitiesOverTLS(t *testing.T) {
	for _, protocol := range []string{"openai_compat", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			var calls atomic.Int64
			fixture := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				prompt, pixels, tool := readProbe(t, r)
				reply := probeReply(t, prompt, pixels, tool)
				if protocol == "openai_compat" {
					if tool {
						arguments := fmt.Sprintf(`{"nonce":%q}`, reply)
						frames(t, w, fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"probe-call","function":{"name":"verify_capability","arguments":%q}}]},"finish_reason":"tool_calls"}]}`, arguments), `[DONE]`)
					} else {
						textFrames(t, w, reply, true)
					}
				} else {
					content := fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":%q}}`, reply)
					reason := "end_turn"
					if tool {
						content = fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"probe-call","name":"verify_capability","input":{"nonce":%q}}}`, reply)
						reason = "tool_use"
					}
					frames(t, w, `{"type":"message_start","message":{"usage":{"input_tokens":10}}}`, content, `{"type":"content_block_stop","index":0}`, fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":5}}`, reason), `{"type":"message_stop"}`)
				}
			}, nil)
			fixture.configuration.Protocol = protocol
			fixture.configuration.SupportsTools = true
			fixture.configuration.SupportsVision = true
			if err := fixture.gateway.Verify(context.Background(), fixture.configuration, nil); err != nil {
				t.Fatal("声明能力未通过实际协议探测", err)
			}
			if calls.Load() != 3 {
				t.Fatal("声明能力未全部探测")
			}
		})
	}
	fixture := newFixture(t, func(w http.ResponseWriter, r *http.Request) { textFrames(t, w, "incorrect", true) }, nil)
	if err := fixture.gateway.Verify(context.Background(), fixture.configuration, nil); !errors.Is(err, ErrVerification) {
		t.Fatal("无关回答通过能力验证", err)
	}
}
