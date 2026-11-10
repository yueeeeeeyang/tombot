package service

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/store"
)

type PolicyRule struct {
	ID          string  `json:"id"`
	BotID       string  `json:"bot_id"`
	ActionClass string  `json:"action_class"`
	Pattern     *string `json:"pattern"`
	Decision    string  `json:"decision"`
	Note        string  `json:"note"`
}

// PolicyDecision 优先 Bot 专属规则，其次精确目标，最后默认规则；不执行用户提供的表达式。
func (s *Approvals) PolicyDecision(ctx context.Context, botID string, action policy.Action) (string, error) {
	return readPolicyDecision(ctx, s.db, botID, action)
}

type policyReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readPolicyDecision(ctx context.Context, reader policyReader, botID string, action policy.Action) (string, error) {
	var decision string
	err := reader.QueryRowContext(ctx, `SELECT decision FROM policy_rules WHERE bot_id IN(?,'*') AND action_class=? AND (pattern=? OR COALESCE(pattern,'')='') ORDER BY (bot_id=?) DESC,(COALESCE(pattern,'')!='') DESC,id DESC LIMIT 1`, botID, action.Class, action.Pattern, botID).Scan(&decision)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return decision, err
}

func writeRule(ctx context.Context, tx *sql.Tx, rule PolicyRule) error {
	if rule.BotID == "" || !policy.ValidAction(rule.ActionClass) || (rule.Decision != "allow" && rule.Decision != "deny") || len(rule.Note) > 1000 || (rule.Pattern != nil && len(*rule.Pattern) > 4096) {
		return badInput("Invalid policy rule.")
	}
	if rule.BotID != "*" {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM bots WHERE id=?)", rule.BotID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return badInput("The policy bot does not exist.")
		}
	}
	// 明确的规则编辑覆盖同一作用域与目标，审计保留决策来源。
	if _, err := tx.ExecContext(ctx, "DELETE FROM policy_rules WHERE bot_id=? AND action_class=? AND COALESCE(pattern,'')=COALESCE(?,'')", rule.BotID, rule.ActionClass, rule.Pattern); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO policy_rules(id,bot_id,action_class,pattern,decision,note,created_at) VALUES(?,?,?,?,?,?,?)", rule.ID, rule.BotID, rule.ActionClass, rule.Pattern, rule.Decision, rule.Note, time.Now().UnixMilli())
	return err
}
func (s *Approvals) SaveRule(ctx context.Context, rule PolicyRule, actor string) (PolicyRule, error) {
	rule.ID = ids.New()
	err := store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if err := writeRule(ctx, tx, rule); err != nil {
			return err
		}
		return store.Audit(ctx, tx, actor, "policy.save", rule.ID, map[string]string{"bot_id": rule.BotID, "action_class": rule.ActionClass, "decision": rule.Decision})
	})
	return rule, err
}
func (s *Approvals) Rules(ctx context.Context, cursor string, limit int) ([]PolicyRule, error) {
	if limit < 1 || limit > 200 {
		return nil, badInput("Limit must be between 1 and 200.")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id,bot_id,action_class,pattern,decision,COALESCE(note,'') FROM policy_rules WHERE (?='' OR id<?) ORDER BY id DESC LIMIT ?", cursor, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rules := []PolicyRule{}
	for rows.Next() {
		var rule PolicyRule
		if err = rows.Scan(&rule.ID, &rule.BotID, &rule.ActionClass, &rule.Pattern, &rule.Decision, &rule.Note); err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}
func (s *Approvals) DeleteRule(ctx context.Context, id, actor string) error {
	return store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if err := store.RequireChanged(tx.ExecContext(ctx, "DELETE FROM policy_rules WHERE id=?", id)); err != nil {
			return err
		}
		return store.Audit(ctx, tx, actor, "policy.delete", id, map[string]string{})
	})
}
