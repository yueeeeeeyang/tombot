package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

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
	ctx := context.Background()
	if _, err := e.approvals.SaveRule(ctx, service.PolicyRule{BotID: bot.ID, ActionClass: "user.question", Decision: "allow"}, "user"); err != nil {
		t.Fatal(err)
	}
	callID := ids.New()
	done := make(chan error, 1)
	go func() {
		result, err := e.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot, UserWaiter: directUserWaiter{}}, domain.ToolUseBlock{ID: callID, Name: "ask_user", Input: map[string]any{"question": "采用哪种格式？", "options": []string{"简报", "表格"}}})
		var answer domain.QuestionAnswer
		if err == nil {
			err = json.Unmarshal([]byte(result.Content), &answer)
		}
		if err == nil && (result.IsError || answer.Choice == nil || *answer.Choice != "表格" || answer.Text != "表格") {
			err = fmt.Errorf("实际答案未回灌：%+v", result)
		}
		done <- err
	}()
	question := pending(t, e)
	if question.ActionClass != "user.question" {
		t.Fatal("提问进入了普通审批", question)
	}
	select {
	case err := <-done:
		t.Fatal("策略允许替用户回答", err)
	default:
	}
	if err := e.approvals.Decide(ctx, question.ID, service.ApprovalDecision{Decision: "approve", Note: "表格"}, "user"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("回答未恢复工具执行")
	}
	var status, output string
	if err := e.db.QueryRow("SELECT status,output_json FROM tool_calls WHERE id=?", callID).Scan(&status, &output); err != nil || status != "succeeded" || !strings.Contains(output, "表格") {
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
	if !result.IsError || !strings.Contains(result.Content, "USER_QUESTION_UNAVAILABLE") {
		t.Fatal(result)
	}
	for _, invalid := range []map[string]any{{"question": "", "options": []string{}}, {"question": "问题", "options": []string{"同项", "同项"}}, {"question": "问题"}} {
		if result = execute(t, e, bot, run, "ask_user", invalid); !result.IsError || strings.Contains(result.Content, "USER_QUESTION_UNAVAILABLE") {
			t.Fatal("无效参数没有提前拒绝", result)
		}
	}
	if _, err := e.approvals.SaveRule(context.Background(), service.PolicyRule{BotID: bot.ID, ActionClass: "user.question", Decision: "deny"}, "user"); err != nil {
		t.Fatal(err)
	}
	result = execute(t, e, bot, run, "ask_user", question)
	if !result.IsError || !strings.Contains(result.Content, "TOOL_DENIED") {
		t.Fatal(result)
	}
	questions, err := e.approvals.List(context.Background(), "pending", "", 20)
	if err != nil || len(questions) != 0 {
		t.Fatal("无效或拒绝提问仍产生待办", questions, err)
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
	done := make(chan error, 1)
	go func() {
		result, err := e.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot}, domain.ToolUseBlock{ID: ids.New(), Name: "conversation_read", Input: map[string]any{"conversation_id": group.ConversationID}})
		if err == nil && (!result.IsError || !strings.Contains(result.Content, "CONVERSATION_FORBIDDEN")) {
			err = fmt.Errorf("审批后仍可读取已移出的群：%+v", result)
		}
		done <- err
	}()
	approval := pending(t, e)
	if _, err = e.db.Exec("DELETE FROM group_members WHERE group_id=? AND bot_id=?", group.ID, bot.ID); err != nil {
		t.Fatal(err)
	}
	if err = e.approvals.Decide(ctx, approval.ID, service.ApprovalDecision{Decision: "approve"}, "user"); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("会话读取审批未结束")
	}
}
