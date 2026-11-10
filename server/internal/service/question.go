package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/store"
)

// NormalizeQuestion 约束问题、选项和重复项；不接受空白正文或空字符。
func NormalizeQuestion(input domain.UserQuestion) (domain.UserQuestion, error) {
	input.Question = strings.TrimSpace(input.Question)
	valid := func(value string, limit int) bool {
		return value != "" && len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
	}
	if !valid(input.Question, 4000) || len(input.Options) > 8 {
		return input, badInput("Use a question up to 4000 UTF-8 bytes and at most 8 options.")
	}
	options := make([]string, 0, len(input.Options))
	seen := map[string]bool{}
	for _, option := range input.Options {
		option = strings.TrimSpace(option)
		if !valid(option, 200) || seen[option] {
			return input, badInput("Options must be unique, nonempty strings up to 200 UTF-8 bytes.")
		}
		seen[option] = true
		options = append(options, option)
	}
	input.Options = options
	return input, nil
}

func noUserResponse() error {
	return domain.NewError(408, "USER_NO_RESPONSE", "The user skipped this question or did not respond before it expired. Do not invent an answer.")
}

// AskUser 持久化问题后等待单次回答，取消会使待办过期；明确不使用策略允许来代替用户输入。
func (s *Approvals) AskUser(ctx context.Context, run domain.Run, callID string, input domain.UserQuestion) (domain.QuestionAnswer, error) {
	input, err := NormalizeQuestion(input)
	if err != nil {
		return domain.QuestionAnswer{}, err
	}
	action := policy.Action{Class: "user.question", Pattern: run.TurnID, Risk: "low", Title: "等待用户回答", ForceApproval: true, Details: map[string]any{"question": input.Question, "options": input.Options}}
	return s.requestWait(ctx, run, callID, action, s.questionTimeout)
}

// decideQuestion 将同回合的一批可见问题原子回答；成员问题变化时拒绝旧快照，避免替用户回答未看见的新问题。
func decideQuestion(ctx context.Context, tx *sql.Tx, id string, input ApprovalDecision, actor, runID, conversationID string) error {
	if input.Remember != nil || !utf8.ValidString(input.Note) || strings.ContainsRune(input.Note, 0) || input.Decision == "approve" && strings.TrimSpace(input.Note) == "" || input.Decision == "deny" && input.Note != "" || !input.ApplyToTurn && len(input.QuestionIDs) > 0 || len(input.QuestionIDs) > domain.MaxGroupMembers {
		return badInput("Answer a question with nonempty note text, or deny without a note. Answers cannot create policy rules.")
	}
	var turnID string
	if err := tx.QueryRowContext(ctx, "SELECT turn_id FROM runs WHERE id=? AND status='waiting_approval'", runID).Scan(&turnID); err != nil {
		return notFound(err)
	}
	type pendingQuestion struct{ id, encoded string }
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.detail_json FROM approvals a JOIN runs r ON r.id=a.run_id WHERE r.turn_id=? AND a.action_class='user.question' AND a.status='pending' AND a.expires_at>? ORDER BY a.id LIMIT ?`, turnID, time.Now().UnixMilli(), domain.MaxGroupMembers+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	questions := []pendingQuestion{}
	for rows.Next() {
		var question pendingQuestion
		if err = rows.Scan(&question.id, &question.encoded); err != nil {
			return err
		}
		questions = append(questions, question)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if len(questions) == 0 || len(questions) > domain.MaxGroupMembers {
		return domain.NewError(409, "USER_QUESTIONS_CHANGED", "The pending questions changed. Reload them before answering.")
	}
	if questions[0].id != id {
		return domain.NewError(409, "USER_QUESTION_QUEUED", "Answer the oldest pending question in this turn first.")
	}
	if input.ApplyToTurn {
		seen := map[string]bool{}
		for _, questionID := range input.QuestionIDs {
			if seen[questionID] {
				return badInput("Question IDs must be unique.")
			}
			seen[questionID] = true
		}
		if len(seen) != len(questions) {
			return domain.NewError(409, "USER_QUESTIONS_CHANGED", "The pending questions changed. Reload them before answering.")
		}
		for _, question := range questions {
			if !seen[question.id] {
				return domain.NewError(409, "USER_QUESTIONS_CHANGED", "The pending questions changed. Reload them before answering.")
			}
		}
	} else {
		questions = questions[:1]
	}
	for _, question := range questions {
		status := "denied"
		var reply any
		if input.Decision == "approve" {
			var stored struct {
				Details domain.UserQuestion `json:"details"`
			}
			if err = json.Unmarshal([]byte(question.encoded), &stored); err != nil {
				return err
			}
			inputQuestion, err := NormalizeQuestion(stored.Details)
			if err != nil {
				return err
			}
			answer := domain.QuestionAnswer{Text: strings.TrimSpace(input.Note)}
			for _, option := range inputQuestion.Options {
				if answer.Text == option {
					choice := option
					answer.Choice = &choice
					break
				}
			}
			encoded, err := json.Marshal(answer)
			if err != nil {
				return err
			}
			reply = string(encoded)
			status = "approved"
		}
		if err = store.RequireChanged(tx.ExecContext(ctx, "UPDATE approvals SET status=?,answer_json=?,decided_at=? WHERE id=? AND status='pending'", status, reply, time.Now().UnixMilli(), question.id)); err != nil {
			return err
		}
		if err = store.Audit(ctx, tx, actor, "approval.decide", question.id, map[string]any{"decision": input.Decision, "apply_to_turn": input.ApplyToTurn, "question_count": len(questions)}); err != nil {
			return err
		}
		if _, err = bus.Append(ctx, tx, "approval.resolved", conversationID, map[string]string{"approval_id": question.id, "status": status, "turn_id": turnID}); err != nil {
			return err
		}
	}
	return nil
}
