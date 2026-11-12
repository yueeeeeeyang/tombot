package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
)

// TestNativeTerminalControl 验证用户与模型先后在同一真实 PTY 操作，命令需经正式审批入口。
func TestNativeTerminalControl(t *testing.T) {
	if os.Getenv("TOM_TEST_NATIVE_TERMINAL") != "1" {
		t.Skip("未启用原生终端验收")
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []struct {
				Role    string
				Content json.RawMessage
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Messages) == 0 {
			t.Error("终端模型请求无效", err)
			http.Error(w, "invalid request", 400)
			return
		}
		step := calls.Add(1)
		switch step {
		case 1, 2:
			args := map[string]any{"action": "list"}
			if step == 2 {
				last := input.Messages[len(input.Messages)-1]
				var content string
				var sessions []service.Terminal
				if last.Role != "tool" || json.Unmarshal(last.Content, &content) != nil || json.Unmarshal([]byte(content), &sessions) != nil || len(sessions) != 1 || sessions[0].Controller != "bot" {
					t.Error("模型未发现已交还的终端")
					http.Error(w, "invalid terminal list", 400)
					return
				}
				args = map[string]any{"action": "write", "id": sessions[0].ID, "data": "printf 'bot=%d\\n' \"$((TOM_PANEL_VALUE + 2))\"\r"}
			}
			writeToolChatReply(t, w, "", domain.ToolUseBlock{ID: fmt.Sprintf("terminal-%d", step), Name: "terminal_session", Input: args})
		case 3:
			last := input.Messages[len(input.Messages)-1]
			var content string
			var terminal service.Terminal
			if last.Role != "tool" || json.Unmarshal(last.Content, &content) != nil || json.Unmarshal([]byte(content), &terminal) != nil || terminal.RunID == "" || terminal.Status != "running" {
				t.Error("模型未收到成功的终端写入结果")
				http.Error(w, "invalid write result", 400)
				return
			}
			writeToolChatReply(t, w, "已完成终端协作。")
		default:
			t.Error("模型多次重放终端操作")
			http.Error(w, "unexpected step", 400)
			return
		}
	}))
	defer upstream.Close()
	server, bot, device := toolChatFixture(t, upstream.URL, []string{"terminal_session"})
	runNativeBotScenario(t, server, bot, device, "terminal_control_test.dart")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	records, err := server.Terminals.List(ctx, bot.ID, "", 5)
	if err != nil || len(records) != 1 || records[0].Status != "closed" || records[0].Owner != "user" || calls.Load() != 3 {
		t.Fatal("终端归属、关闭状态或模型调用次数错误", err, calls.Load())
	}
}
