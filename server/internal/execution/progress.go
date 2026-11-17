package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/botpersona"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/groupchat"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/provider"
)

type progressTool struct{ executor *Service }

const typingSummary = "正在输入…"

// setReplyTyping 只发布生成阶段变化，不发送正文片段；状态入库后重开聊天仍能恢复。
// 回到工具或思考阶段时只清除输入状态，保留模型已报告的实际工作摘要。
func (service *Service) setReplyTyping(ctx context.Context, lease Lease, typing bool) error {
	summary := typingSummary
	if !typing {
		summary = "正在思考…"
	}
	return service.worker(ctx, lease, false, func(tx *sql.Tx) error {
		if err := available(ctx, tx, lease); err != nil {
			return err
		}
		var id string
		err := tx.QueryRowContext(ctx, `UPDATE tombot.runs SET progress_summary=$3 WHERE user_id=$1 AND id=$2 AND progress_summary<>$3 AND ($4 OR progress_summary=$5) RETURNING id`, lease.UserID, lease.RunID, summary, typing, typingSummary).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return event(ctx, tx, lease.UserID, "run.progress", lease.RunID, map[string]string{"bot_id": lease.BotID, "conversation_id": lease.ConversationID, "text": summary})
	})
}

func (progressTool) Schema() provider.ToolSchema { return botpersona.ProgressSchema() }
func (progressTool) UsesComputer() bool          { return false }
func (progressTool) RequiresVision() bool        { return false }
func (tool progressTool) Execute(ctx context.Context, invocation Invocation) (botagent.ToolResult, error) {
	var input any
	if json.Unmarshal([]byte(invocation.Call.Arguments), &input) != nil {
		return botagent.ToolResult{IsError: true, Content: "INVALID_PROGRESS: Provide exactly one summary string."}, nil
	}
	summary, ok := botpersona.ProgressSummary(input)
	if !ok {
		return botagent.ToolResult{IsError: true, Content: "INVALID_PROGRESS: Provide one plain Chinese sentence of 1 to 60 characters without line breaks."}, nil
	}
	err := tool.executor.worker(ctx, invocation.Lease, false, func(tx *sql.Tx) error {
		lease := invocation.Lease
		if _, err := tx.ExecContext(ctx, `UPDATE tombot.runs SET progress_summary=$3 WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID, summary); err != nil {
			return err
		}
		return event(ctx, tx, lease.UserID, "run.progress", lease.RunID, map[string]string{"bot_id": lease.BotID, "conversation_id": lease.ConversationID, "text": summary})
	})
	return botagent.ToolResult{Content: "Progress displayed. Continue the task."}, err
}

// Activity 读取当前会话的活动；单聊展示所属猫咪跨会话的优先运行，群聊按成员返回。
// 空列表表示没有活动运行，不能用其他用户的会话查询。
func (service *Service) Activity(ctx context.Context, principal identity.Principal, conversationID string) ([]Run, error) {
	if !validID(conversationID) {
		return nil, ErrInvalidInput
	}
	runs := []Run{}
	err := service.user(ctx, principal, false, func(tx *sql.Tx) error {
		var kind string
		if err := tx.QueryRowContext(ctx, `SELECT kind FROM tombot.conversations WHERE user_id=$1 AND id=$2`, principal.UserID, conversationID).Scan(&kind); err != nil {
			return err
		}
		query := `SELECT ` + runColumns + ` FROM tombot.runs WHERE user_id=$1 AND conversation_id=$2 AND status IN ('queued','running','canceling') ORDER BY id DESC LIMIT $3`
		limit := 1
		if kind == "direct" {
			query = `SELECT ` + runColumns + ` FROM tombot.runs WHERE user_id=$1 AND id IN (SELECT run_id FROM tombot.jobs WHERE user_id=$1 AND status<>'finished') AND bot_id=(SELECT bot_id FROM tombot.conversations WHERE user_id=$1 AND id=$2) AND status IN ('queued','running','canceling') ORDER BY CASE WHEN status='running' THEN 0 ELSE 1 END,id DESC LIMIT $3`
		}
		if kind == "group" {
			query = `SELECT DISTINCT ON (bot_id) ` + runColumns + ` FROM tombot.runs WHERE user_id=$1 AND conversation_id=$2 AND status IN ('queued','running','canceling') ORDER BY bot_id,CASE WHEN status='running' THEN 0 ELSE 1 END,id DESC LIMIT $3`
			limit = groupchat.MaxMembers
		}
		rows, err := tx.QueryContext(ctx, query, principal.UserID, conversationID, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			run, err := scanRun(rows)
			if err != nil {
				return errors.Join(err, rows.Close())
			}
			runs = append(runs, run)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	return runs, err
}

// BotActivities 返回每只猫咪当前优先展示的活动运行，统一覆盖聊天、私信、群聊和后台任务。
// 按电脑桌面总数限制结果；等待私信而没有活动运行的猫咪不会被标为工作中。
func (service *Service) BotActivities(ctx context.Context, principal identity.Principal) ([]Run, error) {
	runs := []Run{}
	err := service.user(ctx, principal, false, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT ON (bot_id) `+runColumns+` FROM tombot.runs WHERE user_id=$1 AND id IN (SELECT run_id FROM tombot.jobs WHERE user_id=$1 AND status<>'finished') AND status IN ('queued','running','canceling') ORDER BY bot_id,CASE WHEN status='running' THEN 0 ELSE 1 END,id DESC LIMIT $2`, principal.UserID, computer.MaxDesktops)
		if err != nil {
			return err
		}
		for rows.Next() {
			run, err := scanRun(rows)
			if err != nil {
				return errors.Join(err, rows.Close())
			}
			runs = append(runs, run)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	return runs, err
}
