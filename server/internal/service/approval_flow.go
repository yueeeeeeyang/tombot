package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/store"
)

type RememberDecision struct {
	Scope string `json:"scope"`
}
type ApprovalDecision struct {
	Decision    string            `json:"decision"`
	Remember    *RememberDecision `json:"remember"`
	Note        string            `json:"note"`
	ApplyToTurn bool              `json:"apply_to_turn"`
	QuestionIDs []string          `json:"question_ids"`
}

// RequestWait 先持久化待办和等待状态，再监听事件；取消及超时都使待办过期而不执行动作。
func (s *Approvals) RequestWait(ctx context.Context, run domain.Run, callID string, action policy.Action) error {
	_, err := s.requestWait(ctx, run, callID, action, s.timeout)
	return err
}

func (s *Approvals) requestWait(ctx context.Context, run domain.Run, callID string, action policy.Action, duration time.Duration) (answer domain.QuestionAnswer, result error) {
	if !policy.ValidAction(action.Class) || action.Title == "" {
		return answer, badInput("Invalid approval action.")
	}
	if _, err := policy.NeedsApproval("strict", action.Risk); err != nil {
		return answer, err
	}
	encoded, err := json.Marshal(action)
	if err != nil {
		return answer, err
	}
	id := ids.New()
	expires := time.Now().Add(duration)
	create, cancelCreate := context.WithTimeout(ctx, 10*time.Second)
	err = store.Transact(create, s.db, func(tx *sql.Tx) error {
		ctx := create
		if action.Class == "user.question" {
			var count int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM approvals WHERE status='pending' AND action_class='user.question'").Scan(&count); err != nil {
				return err
			}
			if count >= 1024 {
				return domain.NewError(409, "USER_QUESTION_CAPACITY", "Too many questions are awaiting an answer.")
			}
		}
		if err := store.RequireChanged(tx.ExecContext(ctx, "UPDATE runs SET status='waiting_approval' WHERE id=? AND status='running'", run.ID)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE turns SET status='waiting_approval' WHERE id=?", run.TurnID); err != nil {
			return err
		}
		if err := store.RequireChanged(tx.ExecContext(ctx, "UPDATE tool_calls SET status='awaiting_approval',approval_id=? WHERE id=? AND run_id=? AND status='pending'", id, callID, run.ID)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO approvals(id,run_id,tool_call_id,bot_id,action_class,title,detail_json,risk_level,reviewer_note,status,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,'pending',?,?)`, id, run.ID, callID, run.BotID, action.Class, action.Title, string(encoded), action.Risk, action.ReviewerNote, expires.UnixMilli(), time.Now().UnixMilli()); err != nil {
			return err
		}
		if _, err := bus.Append(ctx, tx, "run.status", run.ConversationID, map[string]string{"run_id": run.ID, "turn_id": run.TurnID, "bot_id": run.BotID, "status": "waiting_approval"}); err != nil {
			return err
		}
		if err := AppendToolActivity(ctx, tx, run.ConversationID, callID, "tool.status"); err != nil {
			return err
		}
		if _, err := bus.Append(ctx, tx, "approval.requested", run.ConversationID, map[string]any{"approval": map[string]any{"id": id, "title": action.Title, "run_id": run.ID, "bot_id": run.BotID, "risk_level": action.Risk}}); err != nil {
			return err
		}
		return AppendConversationEvent(ctx, tx, run.ConversationID)
	})
	cancelCreate()
	if err != nil {
		return answer, err
	}
	s.events.Notify()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := store.Transact(cleanup, s.db, func(tx *sql.Tx) error {
			if err := s.expire(cleanup, tx, id, run.ConversationID); err != nil {
				return err
			}
			restored, err := tx.ExecContext(cleanup, "UPDATE runs SET status='running' WHERE id=? AND status='waiting_approval'", run.ID)
			if err != nil {
				return err
			}
			restoredCount, err := restored.RowsAffected()
			if err != nil {
				return err
			}
			if restoredCount > 0 {
				if _, err := bus.Append(cleanup, tx, "run.status", run.ConversationID, map[string]string{"run_id": run.ID, "turn_id": run.TurnID, "bot_id": run.BotID, "status": "running"}); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(cleanup, "UPDATE turns SET status='running' WHERE id=? AND status='waiting_approval' AND NOT EXISTS(SELECT 1 FROM runs WHERE turn_id=? AND status='waiting_approval')", run.TurnID, run.TurnID); err != nil {
				return err
			}
			// 在审批结束与外部动作开始之间仍须复查文件和策略，不能提前向客户端报告工具已执行。
			changed, err := tx.ExecContext(cleanup, "UPDATE tool_calls SET status='pending' WHERE id=? AND status='awaiting_approval'", callID)
			if err != nil {
				return err
			}
			count, err := changed.RowsAffected()
			if err != nil {
				return err
			}
			if count > 0 {
				if err := AppendToolActivity(cleanup, tx, run.ConversationID, callID, "tool.status"); err != nil {
					return err
				}
			}

			return AppendConversationEvent(cleanup, tx, run.ConversationID)
		})
		if err != nil {
			// 状态清理失败必须上抛，不能被原有的审批超时或拒绝错误掩盖。
			result = fmt.Errorf("审批状态清理失败: %w", err)
			return
		}
		s.events.Notify()
	}()
	timer := time.NewTimer(time.Until(expires))
	defer timer.Stop()
	for {
		wake := s.events.Wake()
		var status, reply string
		read, cancelRead := context.WithTimeout(ctx, 5*time.Second)
		err = s.db.QueryRowContext(read, "SELECT status,COALESCE(answer_json,'') FROM approvals WHERE id=?", id).Scan(&status, &reply)
		cancelRead()
		if err != nil {
			return answer, err
		}
		switch status {
		case "approved":
			if action.Class == "user.question" {
				if err = json.Unmarshal([]byte(reply), &answer); err != nil {
					return answer, err
				}
				if answer.Text == "" {
					return answer, errors.New("已回答的问题缺少回答正文")
				}
			}
			return answer, ctx.Err()
		case "denied":
			if action.Class == "user.question" {
				return answer, noUserResponse()
			}
			return answer, domain.NewError(403, "TOOL_DENIED", "The user denied this tool action.")
		case "expired":
			if action.Class == "user.question" {
				return answer, noUserResponse()
			}
			return answer, domain.NewError(408, "APPROVAL_TIMEOUT", "Approval expired without permission to execute.")
		}
		select {
		case <-ctx.Done():
			return answer, ctx.Err()
		case <-timer.C:
			if action.Class == "user.question" {
				return answer, noUserResponse()
			}
			return answer, domain.NewError(408, "APPROVAL_TIMEOUT", "Approval expired without permission to execute.")
		case <-wake:
		}
	}
}

func (s *Approvals) expire(ctx context.Context, tx *sql.Tx, id, conversationID string) error {
	result, err := tx.ExecContext(ctx, "UPDATE approvals SET status='expired',decided_at=? WHERE id=? AND status='pending'", time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return nil
	}
	_, err = bus.Append(ctx, tx, "approval.resolved", conversationID, map[string]string{"approval_id": id, "status": "expired"})
	return err
}

// Decide 用条件状态更新排除重复点击及多设备竞争；规则记忆与审批结果在同一事务提交。
func (s *Approvals) Decide(ctx context.Context, id string, input ApprovalDecision, actor string) error {
	if (input.Decision != "approve" && input.Decision != "deny") || len(input.Note) > 1000 || (input.Remember != nil && input.Remember.Scope != "bot" && input.Remember.Scope != "global") {
		return badInput("Invalid approval decision.")
	}
	expired := false
	err := store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		var status, botID, class, encoded, conversationID string
		var runID sql.NullString
		var expires int64
		if err := tx.QueryRowContext(ctx, `SELECT a.status,COALESCE(a.bot_id,''),a.action_class,a.detail_json,a.expires_at,COALESCE(r.conversation_id,''),a.run_id FROM approvals a LEFT JOIN runs r ON r.id=a.run_id WHERE a.id=?`, id).Scan(&status, &botID, &class, &encoded, &expires, &conversationID, &runID); err != nil {
			return notFound(err)
		}
		if status != "pending" {
			return domain.NewError(409, "APPROVAL_RESOLVED", "This approval is no longer pending.")
		}
		if expires <= time.Now().UnixMilli() {
			expired = true
			return s.expire(ctx, tx, id, conversationID)
		}
		if class == "user.question" {
			return decideQuestion(ctx, tx, id, input, actor, runID.String, conversationID)
		}
		if input.ApplyToTurn || len(input.QuestionIDs) > 0 {
			return badInput("Bulk question answers only apply to user questions.")
		}
		status = "approved"
		if input.Decision == "deny" {
			status = "denied"
		}
		if _, err := tx.ExecContext(ctx, "UPDATE approvals SET status=?,decided_at=? WHERE id=? AND status='pending'", status, time.Now().UnixMilli(), id); err != nil {
			return err
		}
		if input.Remember != nil {
			var action policy.Action
			if err := json.Unmarshal([]byte(encoded), &action); err != nil {
				return err
			}
			if action.ForceApproval {
				return badInput("This action requires an individual decision and cannot create a remembered rule.")
			}
			if input.Remember.Scope == "global" {
				botID = "*"
			}
			decision := "allow"
			if status == "denied" {
				decision = "deny"
			}
			if err := writeRule(ctx, tx, PolicyRule{ID: ids.New(), BotID: botID, ActionClass: class, Pattern: &action.Pattern, Decision: decision, Note: input.Note}); err != nil {
				return err
			}
		}
		if !runID.Valid {
			if class != "routine.create" {
				return errors.New("无执行归属的审批类型无效")
			}
			if status == "approved" {
				if err := applyRoutineChange(ctx, tx, id, actor, time.Now().UTC()); err != nil {
					return err
				}
			}
		}
		if err := store.Audit(ctx, tx, actor, "approval.decide", id, map[string]string{"decision": input.Decision}); err != nil {
			return err
		}
		_, err := bus.Append(ctx, tx, "approval.resolved", conversationID, map[string]string{"approval_id": id, "status": status})
		return err
	})
	if err == nil {
		s.events.Notify()
		if expired {
			return domain.NewError(409, "APPROVAL_EXPIRED", "This approval has expired.")
		}
	}
	return err
}
