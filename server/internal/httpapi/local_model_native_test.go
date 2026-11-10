package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
)

// verifyNativeModelChat 让正式聊天组件继续已有真实模型会话，再中断已开始流式输出的下一轮。
func verifyNativeModelChat(t *testing.T, server *Server, bot domain.Bot, device string) {
	t.Helper()
	runNativeBotScenario(t, server, bot, device, "local_model_test.dart")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	snap, err := server.Conversations.Snapshot(ctx, bot.ConversationID)
	if err != nil || snap.ActiveTurnID != nil {
		t.Fatal("原生会话仍有未收尾任务", err)
	}
	var answered, partial bool
	for _, m := range snap.Messages {
		if m.SenderKind != "bot" {
			continue
		}
		for _, b := range m.Blocks {
			if b.Kind == "text" && m.Status == "complete" && strings.Contains(b.Text, "14") {
				answered = true
			}
			if b.Kind == "text" && (m.Status == "canceled" || m.Status == "interrupted") && b.Text != "" {
				partial = true
			}
		}
	}
	if !answered || !partial {
		t.Fatal("原生追问回复或停止前的部分正文缺失")
	}
}
