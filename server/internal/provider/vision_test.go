package provider

import (
	"encoding/base64"
	"encoding/json"
	"io.yueyang/tom/internal/domain"
	"strings"
	"testing"
)

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
