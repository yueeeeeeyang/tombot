package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/agent"
	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provision"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestAttachmentVisionHTTP(t *testing.T) {
	for _, protocol := range []string{"openai_compat", "anthropic"} {
		t.Run(protocol, func(t *testing.T) { verifyAttachmentVision(t, protocol, "") })
	}
}

func TestRealChromiumScreenshotToVisionModel(t *testing.T) {
	archive := os.Getenv("TOM_TEST_BROWSER_ARCHIVE")
	if archive == "" {
		t.Skip("未设置浏览器归档，跳过真实截图视觉输入验收")
	}
	verifyAttachmentVision(t, "openai_compat", archive)
}

// verifyAttachmentVision 验收上传、消息提交、两种协议与截图回灌，直接读取上游收到的像素和数据库快照。
func verifyAttachmentVision(t *testing.T, protocol, archive string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	var factory func(context.Context, browser.Options) (service.BrowserEngine, error)
	if archive != "" {
		root := t.TempDir()
		if err := provision.EnsureBrowser(ctx, root, archive); err != nil {
			t.Fatal(err)
		}
		factory = func(ctx context.Context, options browser.Options) (service.BrowserEngine, error) {
			options.Executable = provision.BrowserPath(root)
			options.ProfileDir = filepath.Join(root, "profile")
			return browser.New(ctx, options)
		}
	}
	server, code := fixtureWithBrowser(t, true, factory)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := server.Auth.Login(ctx, password, "附件验收", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(server.Handler())
	defer host.Close()
	userPicture := image.NewRGBA(image.Rect(0, 0, 3, 2))
	userPicture.Set(0, 0, color.RGBA{R: 200, A: 255})
	var picture bytes.Buffer
	if err := png.Encode(&picture, userPicture); err != nil {
		t.Fatal(err)
	}
	var tabID atomic.Value
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			t.Error(err)
			return
		}
		var request struct {
			Messages []struct {
				Role    string
				Content json.RawMessage
			}
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
			return
		}
		imageData := base64.StdEncoding.EncodeToString(picture.Bytes())
		if !bytes.Contains(body, []byte(imageData)) || !bytes.Contains(body, []byte("验收文本正文")) {
			t.Error("模型未同时收到上传的原始 PNG 和 UTF-8 文件内容")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		n := calls.Add(1)
		switch {
		case protocol == "anthropic":
			if r.URL.Path != "/v1/messages" || !bytes.Contains(body, []byte(`"media_type":"image/png"`)) {
				t.Error("Anthropic 图片协议错误")
			}
			_, err = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"已收到图片\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
		case n == 1:
			if r.URL.Path != "/v1/chat/completions" || !bytes.Contains(body, []byte("data:image/png;base64,")) {
				t.Error("OpenAI 图片协议错误")
			}
			args := fmt.Sprintf(`{"action":"screenshot","id":"%s"}`, tabID.Load())
			call := map[string]any{"index": 0, "id": "vision-call", "type": "function", "function": map[string]string{"name": "browser", "arguments": args}}
			chunk := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{call}}, "finish_reason": "tool_calls"}}}
			encoded, e := json.Marshal(chunk)
			if e != nil {
				t.Error(e)
				return
			}
			_, err = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", encoded)
		default:
			msgs := request.Messages
			if len(msgs) < 2 || msgs[len(msgs)-2].Role != "tool" || msgs[len(msgs)-1].Role != "user" {
				t.Error("截图打断了工具结果顺序")
				return
			}
			var parts []struct {
				ImageURL struct {
					URL string `json:"url"`
				} `json:"image_url"`
			}
			if e := json.Unmarshal(msgs[len(msgs)-1].Content, &parts); e != nil || len(parts) == 0 {
				t.Error("缺少截图内容块", e)
				return
			}
			raw, e := base64.StdEncoding.DecodeString(strings.TrimPrefix(parts[0].ImageURL.URL, "data:image/jpeg;base64,"))
			if e != nil {
				t.Error(e)
				return
			}
			img, kind, e := image.Decode(bytes.NewReader(raw))
			if e != nil || kind != "jpeg" || img.Bounds().Dx() != browser.ViewportWidth {
				t.Error("模型收到的浏览器截图无法解码", e)
				return
			}
			if archive != "" {
				red, green, blue, _ := img.At(32, 32).RGBA()
				if green <= red || green <= blue {
					t.Error("真实标签的绿色页面未出现在上游截图")
				}
			}
			_, err = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"已收到截图\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}
		if err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	p, err := server.Catalog.CreateProvider(ctx, service.ProviderInput{Kind: protocol, DisplayName: "视觉验收", BaseURL: upstream.URL + "/v1"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	vision := true
	_, err = server.Catalog.CreateModel(ctx, service.ModelInput{ProviderID: p.ID, ModelKey: "vision", DisplayName: "视觉模型", ContextWindow: 32768, MaxOutput: 512, IsDefault: true, SupportsVision: &vision}, "user")
	if err != nil {
		t.Fatal(err)
	}
	bot, err := server.Bots.Create(ctx, service.BotInput{Template: "writer", ToolPolicy: &domain.ToolPolicy{Approval: "loose", AllowedTools: []string{"browser"}}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	website := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, err := io.WriteString(w, `<style>html{background:#119922}</style><title>视觉验收</title>`); err != nil {
			t.Error(err)
		}
	}))
	defer website.Close()
	if protocol == "openai_compat" {
		tab, e := server.Browser.Open(ctx, service.BrowserOpenInput{BotID: bot.ID, URL: website.URL}, nil, "user")
		if e != nil {
			t.Fatal(e)
		}
		tabID.Store(tab.ID)
	}
	var attachments []domain.AttachmentInput
	for _, f := range []struct {
		name    string
		content []byte
	}{{"图.png", picture.Bytes()}, {"材料.txt", []byte("验收文本正文")}} {
		body, contentType := uploadBody(t, f.name, f.content, false)
		req, e := http.NewRequestWithContext(ctx, "POST", host.URL+"/api/v1/fs/upload?bot_id="+bot.ID, bytes.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Authorization", "Bearer "+session.DeviceToken)
		resp, e := host.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		var reference domain.AttachmentInput
		e = json.NewDecoder(resp.Body).Decode(&reference)
		if closeErr := resp.Body.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if e != nil || resp.StatusCode != 201 || reference.SHA256 == "" {
			t.Fatal("上传失败", resp.StatusCode, e)
		}
		attachments = append(attachments, reference)
	}
	response := request(t, server.Handler(), "POST", "/api/v1/conversations/"+bot.ConversationID+"/messages", agent.SendInput{Attachments: attachments}, map[string]string{"Authorization": "Bearer " + session.DeviceToken})
	if response.Code != 202 {
		t.Fatal("附件消息未被接受", response.Code, response.Body.String())
	}
	deadline := time.Now().Add(12 * time.Second)
	for {
		snapshot, err := server.Conversations.Snapshot(ctx, bot.ConversationID)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.ActiveTurnID == nil {
			encoded, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(encoded, []byte(base64.StdEncoding.EncodeToString(picture.Bytes()))) || bytes.Contains(encoded, []byte("验收文本正文")) || !bytes.Contains(encoded, []byte("已收到")) || calls.Load() != map[string]int32{"anthropic": 1, "openai_compat": 2}[protocol] {
				t.Fatal("消息快照、最终回复或模型调用次数不正确", calls.Load())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("附件会话未按期结束", calls.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
