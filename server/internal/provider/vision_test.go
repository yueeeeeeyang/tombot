package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"strings"
	"testing"
)

func TestDesktopImageReferenceUsesActualPixels(t *testing.T) {
	var pixels bytes.Buffer
	if err := png.Encode(&pixels, image.NewRGBA(image.Rect(0, 0, 1280, 800))); err != nil {
		t.Fatal(err)
	}
	ref := &domain.ArtifactRef{ID: ids.New(), BotID: ids.New(), MediaType: "image/png", Width: 1280, Height: 800, Bytes: pixels.Len(), Content: pixels.Bytes()}
	for _, kind := range []string{"anthropic", "openai_compat"} {
		t.Run(kind, func(t *testing.T) {
			request := minimalRequest()
			request.Model.SupportsVision = true
			request.Messages = []Message{
				{Role: "assistant", Blocks: []domain.MessageBlock{{Kind: "tool_use", ToolUse: &domain.ToolUseBlock{ID: "screen", Name: "computer_screenshot", Input: map[string]any{}}}}},
				{Role: "user", Blocks: []domain.MessageBlock{{Kind: "tool_result", ToolResult: &domain.ToolResultBlock{ToolCallID: "screen", Artifact: ref}}}},
			}
			adapter := NewHTTP(domain.Provider{Kind: kind}, nil)
			body, err := adapter.encode(request)
			if err != nil {
				t.Fatal(err)
			}
			encoded := base64.StdEncoding.EncodeToString(ref.Content)
			if !bytes.Contains(body, []byte(encoded)) || !bytes.Contains(body, []byte(ref.ID)) || !bytes.Contains(body, []byte("untrusted screen content")) || !bytes.Contains(body, []byte("image/png")) {
				t.Fatal("桌面图片没有编码实际 PNG 或缺少不可信观察标注")
			}
			ref.Content = nil
			body, err = adapter.encode(request)
			ref.Content = pixels.Bytes()
			if err != nil || bytes.Contains(body, []byte(encoded)) || !bytes.Contains(body, []byte("Image pixels are not included")) {
				t.Fatal("旧图片未明确标注不附带像素", err)
			}
		})
	}
}

// 两个工具可以并行返回；OpenAI 的工具回复必须相邻，Anthropic 的图片必须留在对应的工具结果内。
func TestVisionCapabilitiesAndParallelToolResults(t *testing.T) {
	raw := []byte("workspace screenshot payload")
	encoded := base64.StdEncoding.EncodeToString(raw)
	ref := &domain.ArtifactRef{AttachmentInput: domain.AttachmentInput{Path: "inbox/example/screen.jpg"}, MediaType: "image/jpeg", Bytes: len(raw), Content: raw}
	for _, kind := range []string{"anthropic", "openai_compat"} {
		t.Run(kind, func(t *testing.T) {
			adapter := NewHTTP(domain.Provider{Kind: kind}, nil)
			request := minimalRequest()
			request.System = ""
			request.Messages = []Message{
				{Role: "assistant", Blocks: []domain.MessageBlock{
					{Kind: "tool_use", ToolUse: &domain.ToolUseBlock{ID: "screen", Name: "browser.screenshot", Input: map[string]any{}}},
					{Kind: "tool_use", ToolUse: &domain.ToolUseBlock{ID: "list", Name: "browser.list_tabs", Input: map[string]any{}}},
				}},
				{Role: "user", Blocks: []domain.MessageBlock{{Kind: "tool_result", ToolResult: &domain.ToolResultBlock{ToolCallID: "screen", Content: "saved", Artifact: ref}}}},
				{Role: "user", Blocks: []domain.MessageBlock{{Kind: "tool_result", ToolResult: &domain.ToolResultBlock{ToolCallID: "list", Content: "listed"}}}},
			}
			for _, vision := range []bool{false, true} {
				request.Model.SupportsVision = vision
				body, err := adapter.encode(request)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(body), encoded) != vision || !strings.Contains(string(body), ref.Path) {
					t.Fatal("模型视觉能力与附带内容不符")
				}
				var payload struct {
					Messages []struct {
						Role       string
						Content    json.RawMessage
						ToolCallID string `json:"tool_call_id"`
					}
				}
				if err = json.Unmarshal(body, &payload); err != nil {
					t.Fatal(err)
				}
				msgs := payload.Messages
				if kind == "openai_compat" {
					if len(msgs) != 4 || msgs[1].Role != "tool" || msgs[1].ToolCallID != "screen" || msgs[2].ToolCallID != "list" || msgs[3].Role != "user" || !strings.Contains(string(msgs[3].Content), ref.Path) {
						t.Fatal("截图打断工具回复顺序")
					}
				} else if len(msgs) != 3 || !strings.Contains(string(msgs[1].Content), `"tool_use_id":"screen"`) || !strings.Contains(string(msgs[1].Content), ref.Path) || strings.Contains(string(msgs[2].Content), ref.Path) {
					t.Fatal("截图没有绑定正确的工具结果")
				}
			}
		})
	}
}
