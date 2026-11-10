package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/store"
	"io.yueyang/tom/internal/usage"
)

type Settings struct{ db *sql.DB }

func NewSettings(db *sql.DB) *Settings { return &Settings{db: db} }

type ReviewerPatch struct {
	Enabled Field[*bool]   `json:"enabled"`
	ModelID Field[*string] `json:"model_id"`
}
type SettingsPatch struct {
	Reviewer      *ReviewerPatch      `json:"reviewer"`
	MonthlyBudget *MonthlyBudgetPatch `json:"monthly_budget"`
}

type MonthlyBudgetPatch struct {
	LimitMicro  Field[*int64]  `json:"limit_micro"`
	WarnPercent Field[*int]    `json:"warn_percent"`
	Action      Field[*string] `json:"action"`
	Revision    *int64         `json:"revision"`
}

func readReviewer(row scanner) (domain.ReviewerSettings, error) {
	var settings domain.ReviewerSettings
	var encoded string
	err := row.Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	var stored *struct {
		Enabled *bool   `json:"enabled"`
		ModelID *string `json:"model_id"`
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&stored); err != nil {
		return settings, err
	}
	if stored == nil || stored.Enabled == nil {
		return settings, errors.New("审查配置缺少启用状态")
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return settings, errors.New("审查配置含尾随内容")
	}
	if stored.ModelID != nil && (*stored.ModelID == "" || strings.TrimSpace(*stored.ModelID) != *stored.ModelID || len(*stored.ModelID) > 128) {
		return settings, errors.New("审查模型标识无效")
	}
	settings = domain.ReviewerSettings{Enabled: *stored.Enabled, ModelID: stored.ModelID}
	return settings, nil
}

// Reviewer 返回已保存的配置；未配置时关闭审查，损坏配置不能默认为关闭。
func (s *Settings) Reviewer(ctx context.Context) (domain.ReviewerSettings, error) {
	return readReviewer(s.db.QueryRowContext(ctx, "SELECT value_json FROM settings WHERE key='reviewer'"))
}
func (s *Settings) Get(ctx context.Context) (domain.Settings, error) {
	var settings domain.Settings
	err := store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		settings.Reviewer, err = readReviewer(tx.QueryRowContext(ctx, "SELECT value_json FROM settings WHERE key='reviewer'"))
		if err != nil {
			return err
		}
		settings.MonthlyBudget, err = usage.ReadMonthlyBudget(ctx, tx)
		return err
	})
	return settings, err
}

// Update 在同一事务内合并字段、检查模型引用并审计，不会覆盖并发提交的其他字段。
func (s *Settings) Update(ctx context.Context, patch SettingsPatch, actor string) (domain.Settings, error) {
	var settings domain.Settings
	if patch.Reviewer == nil && patch.MonthlyBudget == nil || patch.Reviewer != nil && (!patch.Reviewer.Enabled.Set && !patch.Reviewer.ModelID.Set) {
		return settings, badInput("Provide at least one setting in each supplied section.")
	}
	if patch.Reviewer != nil && patch.Reviewer.Enabled.Set && patch.Reviewer.Enabled.Value == nil {
		return settings, badInput("Reviewer enabled must be a boolean.")
	}
	err := store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		current, err := readReviewer(tx.QueryRowContext(ctx, "SELECT value_json FROM settings WHERE key='reviewer'"))
		if err != nil {
			return err
		}
		if patch.Reviewer != nil {
			if patch.Reviewer.Enabled.Set {
				current.Enabled = *patch.Reviewer.Enabled.Value
			}
			if patch.Reviewer.ModelID.Set {
				current.ModelID = patch.Reviewer.ModelID.Value
			}
			if current.ModelID != nil {
				id := *current.ModelID
				if id == "" || strings.TrimSpace(id) != id || len(id) > 128 {
					return badInput("Use a model ID or null for automatic selection.")
				}
				var enabled bool
				err = tx.QueryRowContext(ctx, "SELECT p.is_enabled FROM models m JOIN providers p ON p.id=m.provider_id WHERE m.id=?", id).Scan(&enabled)
				if errors.Is(err, sql.ErrNoRows) {
					return domain.NewError(400, "REVIEWER_MODEL_UNAVAILABLE", "Select an available reviewer model.")
				}
				if err != nil {
					return err
				}
				if current.Enabled && !enabled {
					return domain.NewError(400, "REVIEWER_MODEL_UNAVAILABLE", "Enable the reviewer model provider first.")
				}
			}
			encoded, err := json.Marshal(current)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value_json,updated_at) VALUES('reviewer',?,?) ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json,updated_at=excluded.updated_at`, string(encoded), time.Now().UnixMilli()); err != nil {
				return err
			}
			if err = store.Audit(ctx, tx, actor, "settings.update", "reviewer", current); err != nil {
				return err
			}
		}
		settings.Reviewer = current
		settings.MonthlyBudget, err = updateMonthlyBudget(ctx, tx, patch.MonthlyBudget, actor)
		return err
	})
	return settings, err
}

func updateMonthlyBudget(ctx context.Context, tx *sql.Tx, patch *MonthlyBudgetPatch, actor string) (domain.MonthlyBudget, error) {
	budget, err := usage.ReadMonthlyBudget(ctx, tx)
	if err != nil || patch == nil {
		return budget, err
	}
	if patch.Revision == nil || *patch.Revision < 0 || (!patch.LimitMicro.Set && !patch.WarnPercent.Set && !patch.Action.Set) || (patch.WarnPercent.Set && patch.WarnPercent.Value == nil) || (patch.Action.Set && patch.Action.Value == nil) {
		return budget, badInput("Provide a revision and at least one valid monthly budget field.")
	}
	if *patch.Revision != budget.Revision {
		return budget, domain.NewError(409, "MONTHLY_BUDGET_CHANGED", "Reload the monthly budget before saving.")
	}
	if patch.LimitMicro.Set {
		budget.LimitMicro = patch.LimitMicro.Value
	}
	if patch.WarnPercent.Set {
		budget.WarnPercent = *patch.WarnPercent.Value
	}
	if patch.Action.Set {
		budget.Action = *patch.Action.Value
	}
	if err = usage.ValidateMonthlyBudget(budget); err != nil {
		return budget, err
	}
	budget.Revision++
	encoded, err := json.Marshal(budget)
	if err != nil {
		return budget, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value_json,updated_at) VALUES('monthly_budget',?,?) ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json,updated_at=excluded.updated_at`, string(encoded), time.Now().UnixMilli()); err != nil {
		return budget, err
	}
	if err = store.Audit(ctx, tx, actor, "settings.update", "monthly_budget", budget); err != nil {
		return budget, err
	}
	return budget, usage.NotifyMonthly(ctx, tx, time.Now())
}
