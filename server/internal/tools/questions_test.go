package tools

import (
	"context"
	"strings"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/service"
)

type directUserWaiter struct{}

func (directUserWaiter) WaitForUser(ctx context.Context, wait func(context.Context) (domain.QuestionAnswer, error)) (domain.QuestionAnswer, error) {
	return wait(ctx)
}

func TestAskUserAlwaysWaitsForRealAnswerAndPersistsResult(t *testing.T) {
	e, bot, run := fixture(t, "strict")
	bot.ToolPolicy.AllowedTools = append(bot.ToolPolicy.AllowedTools, "ask_user")
	callID := ids.New()
	result, err := e.Execute(context.Background(), domain.ToolInvocation{Run: run, Bot: bot, UserWaiter: directUserWaiter{}}, domain.ToolUseBlock{ID: callID, Name: "ask_user", Input: map[string]any{"question": "采用哪种格式？", "options": []string{"简报", "表格"}}})
	if err != nil || !result.IsError || !strings.Contains(result.Content, "USER_QUESTION_REMOVED") {
		t.Fatal(result, err)
	}
	var status, output string
	if err = e.db.QueryRow("SELECT status,output_json FROM tool_calls WHERE id=?", callID).Scan(&status, &output); err != nil || status != "failed" || !strings.Contains(output, "USER_QUESTION_REMOVED") {
		t.Fatal(status, output, err)
	}
}

func TestAskUserWhitelistPolicyRuntimeAndInputFailures(t *testing.T) {
	e, bot, run := fixture(t, "standard")
	question := map[string]any{"question": "采用哪种格式？", "options": []string{"简报"}}
	result := execute(t, e, bot, run, "ask_user", question)
	if !result.IsError || !strings.Contains(result.Content, "TOOL_NOT_ALLOWED") {
		t.Fatal(result)
	}
	bot.ToolPolicy.AllowedTools = append(bot.ToolPolicy.AllowedTools, "ask_user")
	result = execute(t, e, bot, run, "ask_user", question)
	if !result.IsError || !strings.Contains(result.Content, "USER_QUESTION_REMOVED") {
		t.Fatal(result)
	}
	for _, invalid := range []map[string]any{{"question": "", "options": []string{}}, {"question": "问题", "options": []string{"同项", "同项"}}, {"question": "问题"}} {
		if result = execute(t, e, bot, run, "ask_user", invalid); !result.IsError || strings.Contains(result.Content, "USER_QUESTION_REMOVED") {
			t.Fatal("无效参数没有提前拒绝", result)
		}
	}
}

func TestConversationToolRechecksMembershipAfterApproval(t *testing.T) {
	e, bot, run := fixture(t, "strict")
	bot.ToolPolicy.AllowedTools = append(bot.ToolPolicy.AllowedTools, "conversation_read", "conversation_search")
	ctx := context.Background()
	group, err := service.NewGroups(e.db, e.events).Create(ctx, service.GroupInput{Name: "检索审批测试", Members: []string{bot.ID}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.db.Exec("DELETE FROM group_members WHERE group_id=? AND bot_id=?", group.ID, bot.ID); err != nil {
		t.Fatal(err)
	}
	result, err := e.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot}, domain.ToolUseBlock{ID: ids.New(), Name: "conversation_read", Input: map[string]any{"conversation_id": group.ConversationID}})
	if err != nil || !result.IsError || !strings.Contains(result.Content, "CONVERSATION_FORBIDDEN") {
		t.Fatal("移出群后仍可读取会话", result, err)
	}
}
