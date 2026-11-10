package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/scheduler"
	"io.yueyang/tom/internal/store"
)

// RoutineDispatcher 只接收已持久化的触发标识；运行时须把关联回合与触发状态放入同一事务。
type RoutineDispatcher interface {
	SubmitRoutine(context.Context, string) error
}

type Routines struct {
	db               *sql.DB
	events           *bus.Bus
	dispatcher       RoutineDispatcher
	now              func() time.Time
	startupMu        sync.Mutex
	startupRecovered bool
}

// NewRoutines 绑定索引、事件与入队端口；时钟保留为可注入依赖，测试不依赖真实分钟等待。
func NewRoutines(db *sql.DB, events *bus.Bus, dispatcher RoutineDispatcher) *Routines {
	return &Routines{db: db, events: events, dispatcher: dispatcher, now: time.Now}
}

type RoutineInput struct {
	domain.RoutineDefinition
	IsEnabled *bool `json:"is_enabled"`
	Revision  int64 `json:"revision"`
}
type RoutineProposal struct {
	ApprovalID string `json:"approval_id"`
	RoutineID  string `json:"routine_id"`
}

type RoutinePatch struct {
	Revision             int64                       `json:"revision"`
	Name                 Field[string]               `json:"name"`
	Cron                 Field[string]               `json:"cron"`
	Timezone             Field[string]               `json:"timezone"`
	Prompt               Field[string]               `json:"prompt"`
	Target               Field[domain.RoutineTarget] `json:"target"`
	OverlapPolicy        Field[string]               `json:"overlap_policy"`
	CatchUp              Field[string]               `json:"catch_up"`
	TargetMode           Field[string]               `json:"target_mode"`
	PinnedConversationID Field[string]               `json:"pinned_conversation_id"`
	IsEnabled            Field[*bool]                `json:"is_enabled"`
}

// ProposeUpdate 保留未提供的配置，合并后仍通过同一提案校验和版本检查；读取之后的并发修改会被拒绝。
func (s *Routines) ProposeUpdate(ctx context.Context, id string, patch RoutinePatch, actor string) (RoutineProposal, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return RoutineProposal{}, err
	}
	if current.Revision != patch.Revision {
		return RoutineProposal{}, routineChanged()
	}
	definition := current.RoutineDefinition
	for _, entry := range []struct {
		field  Field[string]
		target *string
	}{
		{patch.Name, &definition.Name}, {patch.Cron, &definition.Cron}, {patch.Timezone, &definition.Timezone}, {patch.Prompt, &definition.Prompt},
		{patch.OverlapPolicy, &definition.OverlapPolicy}, {patch.CatchUp, &definition.CatchUp}, {patch.TargetMode, &definition.TargetMode},
	} {
		if entry.field.Set {
			*entry.target = entry.field.Value
		}
	}
	if patch.Target.Set {
		if definition.Target != patch.Target.Value {
			definition.PinnedConversationID = ""
		}
		definition.Target = patch.Target.Value
	}
	if definition.TargetMode == "new" {
		definition.PinnedConversationID = ""
	}
	if patch.PinnedConversationID.Set {
		definition.PinnedConversationID = patch.PinnedConversationID.Value
	}
	if patch.IsEnabled.Set {
		if patch.IsEnabled.Value == nil {
			return RoutineProposal{}, badInput("Routine enabled must be a boolean.")
		}
		definition.IsEnabled = *patch.IsEnabled.Value
	}
	return s.Propose(ctx, id, RoutineInput{RoutineDefinition: definition, IsEnabled: &definition.IsEnabled, Revision: patch.Revision}, actor)
}

const routineColumns = `id,name,cron,timezone,prompt,COALESCE(bot_id,''),COALESCE(group_id,''),overlap_policy,catch_up,target_mode,COALESCE(pinned_conversation_id,''),is_enabled,revision,next_run_at,last_run_at,created_at,updated_at`

func scanRoutine(row scanner) (r domain.Routine, err error) {
	var next, last sql.NullInt64
	var created, updated int64
	err = row.Scan(&r.ID, &r.Name, &r.Cron, &r.Timezone, &r.Prompt, &r.Target.BotID, &r.Target.GroupID, &r.OverlapPolicy, &r.CatchUp, &r.TargetMode, &r.PinnedConversationID, &r.IsEnabled, &r.Revision, &next, &last, &created, &updated)
	if next.Valid {
		value := time.UnixMilli(next.Int64).UTC()
		r.NextRunAt = &value
	}
	if last.Valid {
		value := time.UnixMilli(last.Int64).UTC()
		r.LastRunAt = &value
	}
	r.CreatedAt = time.UnixMilli(created).UTC()
	r.UpdatedAt = time.UnixMilli(updated).UTC()
	return r, notFound(err)
}

// Get 返回已批准的例程；未批准的配置只存在于待办中，不会提前覆盖当前配置。
func (s *Routines) Get(ctx context.Context, id string) (domain.Routine, error) {
	return scanRoutine(s.db.QueryRowContext(ctx, "SELECT "+routineColumns+" FROM routines WHERE id=?", id))
}

// List 按标识分页并可按一种目标过滤，避免加载全部提示词与历史触发。
func (s *Routines) List(ctx context.Context, botID, groupID, cursor string, limit int) ([]domain.Routine, error) {
	if limit < 1 || limit > 200 || (botID != "" && groupID != "") {
		return nil, badInput("Use one target filter and a limit between 1 and 200.")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+routineColumns+` FROM routines WHERE (?='' OR bot_id=?) AND (?='' OR group_id=?) AND (?='' OR id<?) ORDER BY id DESC LIMIT ?`, botID, botID, groupID, groupID, cursor, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	routines := []domain.Routine{}
	for rows.Next() {
		r, err := scanRoutine(rows)
		if err != nil {
			return nil, err
		}
		routines = append(routines, r)
	}
	return routines, rows.Err()
}

func normalizeRoutine(input RoutineInput, now time.Time) (domain.RoutineDefinition, error) {
	r := input.RoutineDefinition
	r.Name = strings.TrimSpace(r.Name)
	r.Prompt = strings.TrimSpace(r.Prompt)
	r.Cron = strings.TrimSpace(r.Cron)
	if r.Timezone == "" {
		r.Timezone = "Asia/Shanghai"
	}
	if r.OverlapPolicy == "" {
		r.OverlapPolicy = "skip"
	}
	if r.CatchUp == "" {
		r.CatchUp = "once"
	}
	if r.TargetMode == "" {
		r.TargetMode = "new"
	}
	r.IsEnabled = true
	if input.IsEnabled != nil {
		r.IsEnabled = *input.IsEnabled
	}
	if r.Name == "" || len(r.Name) > 200 || strings.ContainsAny(r.Name, "\n\r\x00") || r.Prompt == "" || len(r.Prompt) > 64<<10 || !utf8.ValidString(r.Prompt+r.Name) || strings.ContainsRune(r.Prompt, 0) || (r.Target.BotID == "") == (r.Target.GroupID == "") || len(r.Target.BotID) > 128 || len(r.Target.GroupID) > 128 {
		return r, badInput("Provide a name, prompt, and exactly one bot or group target within the size limits.")
	}
	if (r.OverlapPolicy != "skip" && r.OverlapPolicy != "queue") || (r.CatchUp != "once" && r.CatchUp != "skip") || (r.TargetMode != "new" && r.TargetMode != "pinned") || (r.TargetMode == "new" && r.PinnedConversationID != "") {
		return r, badInput("Use skip or queue overlap, once or skip catch-up, and new or pinned conversation mode.")
	}
	_, err := scheduler.Next(r.Cron, r.Timezone, now)
	return r, err
}

// validateRoutineTarget 在事务中校验归属和归档状态；固定模式省略会话时选该目标的主会话。
func validateRoutineTarget(ctx context.Context, tx *sql.Tx, r *domain.RoutineDefinition) error {
	var primary string
	if r.Target.BotID != "" {
		bot, err := ReadBot(ctx, tx, r.Target.BotID)
		if err != nil {
			return err
		}
		if bot.IsArchived {
			return domain.NewError(409, "BOT_ARCHIVED", "Use an active bot for this routine.")
		}
		primary = bot.ConversationID
	} else {
		group, err := ReadGroup(ctx, tx, r.Target.GroupID)
		if err != nil {
			return err
		}
		primary = group.ConversationID
	}
	if r.TargetMode == "pinned" && r.PinnedConversationID == "" {
		r.PinnedConversationID = primary
	}
	conversation := primary
	if r.TargetMode == "pinned" {
		conversation = r.PinnedConversationID
	}
	var valid bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversations WHERE id=? AND is_archived=0 AND ((kind='direct' AND bot_id=?) OR (kind='group' AND group_id=?)))`, conversation, r.Target.BotID, r.Target.GroupID).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return domain.NewError(400, "ROUTINE_TARGET_INVALID", "The routine conversation must belong to its active target.")
	}
	return nil
}

// Propose 创建有时限的逐次审批；现有例程只接受匹配版本的修改，不提前改变调度状态。
func (s *Routines) Propose(ctx context.Context, id string, input RoutineInput, actor string) (proposal RoutineProposal, err error) {
	now := s.now().UTC()
	definition, err := normalizeRoutine(input, now)
	if err != nil {
		return proposal, err
	}
	if id == "" {
		if input.Revision != 0 {
			return proposal, badInput("A new routine cannot specify a revision.")
		}
		id = ids.New()
	} else if input.Revision < 1 {
		return proposal, badInput("Editing requires the current routine revision.")
	}
	proposal = RoutineProposal{ApprovalID: ids.New(), RoutineID: id}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if input.Revision > 0 {
			current, err := scanRoutine(tx.QueryRowContext(ctx, "SELECT "+routineColumns+" FROM routines WHERE id=?", id))
			if err != nil {
				return err
			}
			if current.Revision != input.Revision {
				return routineChanged()
			}
		}
		if err := validateRoutineTarget(ctx, tx, &definition); err != nil {
			return err
		}
		var pending int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE run_id IS NULL AND status='pending'`).Scan(&pending); err != nil {
			return err
		}
		if pending >= 1000 {
			return domain.NewError(429, "ROUTINE_CAPACITY", "Resolve pending routine approvals before adding more.")
		}
		encoded, err := json.Marshal(definition)
		if err != nil {
			return err
		}
		title := "创建例程：" + definition.Name
		if input.Revision > 0 {
			title = "修改例程：" + definition.Name
		}
		detail, err := json.Marshal(policy.Action{Class: "routine.create", Pattern: id, Risk: "medium", Title: title, ForceApproval: true, Details: map[string]any{"routine_id": id, "revision": input.Revision, "definition": definition}})
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO approvals(id,bot_id,action_class,title,detail_json,risk_level,status,expires_at,created_at) VALUES(?,NULLIF(?,''),'routine.create',?,?,'medium','pending',?,?)`, proposal.ApprovalID, definition.Target.BotID, title, string(detail), now.Add(24*time.Hour).UnixMilli(), now.UnixMilli()); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO routine_changes(approval_id,routine_id,expected_revision,definition_json) VALUES(?,?,?,?)`, proposal.ApprovalID, id, input.Revision, string(encoded)); err != nil {
			return err
		}
		if err = store.Audit(ctx, tx, actor, "routine.propose", id, map[string]any{"approval_id": proposal.ApprovalID, "revision": input.Revision}); err != nil {
			return err
		}
		_, err = bus.Append(ctx, tx, "approval.requested", "", map[string]any{"approval": map[string]any{"id": proposal.ApprovalID, "title": title, "bot_id": definition.Target.BotID, "risk_level": "medium"}})
		return err
	})
	if err == nil {
		s.events.Notify()
	}
	return proposal, err
}

func routineChanged() error {
	return domain.NewError(409, "ROUTINE_CHANGED", "The routine configuration changed. Reload it before proposing another edit.")
}

// applyRoutineChange 与审批决策共用事务；旧版本、目标删除和审计失败均不会留下已批准但未生效的配置。
func applyRoutineChange(ctx context.Context, tx *sql.Tx, approvalID, actor string, now time.Time) error {
	var id, encoded string
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT routine_id,expected_revision,definition_json FROM routine_changes WHERE approval_id=?`, approvalID).Scan(&id, &revision, &encoded); err != nil {
		return err
	}
	var definition domain.RoutineDefinition
	if err := json.Unmarshal([]byte(encoded), &definition); err != nil {
		return err
	}
	if err := validateRoutineTarget(ctx, tx, &definition); err != nil {
		return err
	}
	next, err := scheduler.Next(definition.Cron, definition.Timezone, now)
	if err != nil {
		return err
	}
	var nextValue any
	if definition.IsEnabled {
		nextValue = next.UnixMilli()
	}
	if revision == 0 {
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM routines").Scan(&count); err != nil {
			return err
		}
		if count >= 1000 {
			return domain.NewError(429, "ROUTINE_CAPACITY", "At most 1000 routines are supported.")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO routines(id,name,bot_id,group_id,cron,timezone,prompt,overlap_policy,catch_up,target_mode,pinned_conversation_id,is_enabled,next_run_at,created_at,updated_at) VALUES(?,?,NULLIF(?,''),NULLIF(?,''),?,?,?,?,?,?,NULLIF(?,''),?,?,?,?)`, id, definition.Name, definition.Target.BotID, definition.Target.GroupID, definition.Cron, definition.Timezone, definition.Prompt, definition.OverlapPolicy, definition.CatchUp, definition.TargetMode, definition.PinnedConversationID, definition.IsEnabled, nextValue, now.UnixMilli(), now.UnixMilli())
	} else {
		current, readErr := scanRoutine(tx.QueryRowContext(ctx, "SELECT "+routineColumns+" FROM routines WHERE id=?", id))
		if readErr != nil {
			return readErr
		}
		if current.Revision != revision {
			return routineChanged()
		}
		if current.IsEnabled && definition.IsEnabled && current.Cron == definition.Cron && current.Timezone == definition.Timezone && current.NextRunAt != nil {
			nextValue = current.NextRunAt.UnixMilli()
		}
		_, err = tx.ExecContext(ctx, `UPDATE routines SET name=?,bot_id=NULLIF(?,''),group_id=NULLIF(?,''),cron=?,timezone=?,prompt=?,overlap_policy=?,catch_up=?,target_mode=?,pinned_conversation_id=NULLIF(?,''),is_enabled=?,next_run_at=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, definition.Name, definition.Target.BotID, definition.Target.GroupID, definition.Cron, definition.Timezone, definition.Prompt, definition.OverlapPolicy, definition.CatchUp, definition.TargetMode, definition.PinnedConversationID, definition.IsEnabled, nextValue, now.UnixMilli(), id, revision)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE routine_runs SET status='failed',error_code='ROUTINE_CHANGED',updated_at=? WHERE routine_id=? AND status='pending'`, now.UnixMilli(), id)
		}
	}
	if err != nil {
		return store.ConstraintError(err)
	}
	if err = store.Audit(ctx, tx, actor, "routine.apply", id, map[string]any{"approval_id": approvalID, "revision": revision + 1}); err != nil {
		return err
	}
	_, err = bus.Append(ctx, tx, "routine.updated", "", map[string]any{"routine_id": id, "revision": revision + 1})
	return err
}

// Delete 拒绝仍有活动回合的例程，并使待审批修改过期；已完成转录保留在会话中。
func (s *Routines) Delete(ctx context.Context, id string, revision int64, actor string) error {
	err := store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		current, err := scanRoutine(tx.QueryRowContext(ctx, "SELECT "+routineColumns+" FROM routines WHERE id=?", id))
		if err != nil {
			return err
		}
		if current.Revision != revision {
			return routineChanged()
		}
		var active bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM routine_runs rr JOIN turns t ON t.id=rr.turn_id WHERE rr.routine_id=? AND t.status IN('queued','running','waiting_approval'))`, id).Scan(&active); err != nil {
			return err
		}
		if active {
			return domain.NewError(409, "TURN_IN_PROGRESS", "Stop the active routine turn before deleting its configuration.")
		}
		if _, err = tx.ExecContext(ctx, `UPDATE approvals SET status='expired',decided_at=? WHERE status='pending' AND id IN(SELECT approval_id FROM routine_changes WHERE routine_id=?)`, s.now().UnixMilli(), id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM routine_runs WHERE routine_id=?`, id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM routines WHERE id=?`, id); err != nil {
			return err
		}
		if err = store.Audit(ctx, tx, actor, "routine.delete", id, map[string]int64{"revision": revision}); err != nil {
			return err
		}
		_, err = bus.Append(ctx, tx, "routine.updated", "", map[string]any{"routine_id": id, "deleted": true})
		return err
	})
	if err == nil {
		s.events.Notify()
	}
	return err
}

// Commit 直接创建例程。桌面不再经过审批。
func (s *Routines) Commit(ctx context.Context, input RoutineInput, actor string) (domain.Routine, error) {
	now := s.now().UTC()
	definition, err := normalizeRoutine(input, now)
	if err != nil {
		return domain.Routine{}, err
	}
	if input.Revision != 0 {
		return domain.Routine{}, badInput("A new routine cannot specify a revision.")
	}
	id := ids.New()
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if err := validateRoutineTarget(ctx, tx, &definition); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM routines").Scan(&count); err != nil {
			return err
		}
		if count >= 1000 {
			return domain.NewError(429, "ROUTINE_CAPACITY", "At most 1000 routines are supported.")
		}
		next, err := scheduler.Next(definition.Cron, definition.Timezone, now)
		if err != nil {
			return err
		}
		var nextValue any
		if definition.IsEnabled {
			nextValue = next.UnixMilli()
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO routines(id,name,bot_id,group_id,cron,timezone,prompt,overlap_policy,catch_up,target_mode,pinned_conversation_id,is_enabled,next_run_at,created_at,updated_at) VALUES(?,?,NULLIF(?,''),NULLIF(?,''),?,?,?,?,?,?,NULLIF(?,''),?,?,?,?)`, id, definition.Name, definition.Target.BotID, definition.Target.GroupID, definition.Cron, definition.Timezone, definition.Prompt, definition.OverlapPolicy, definition.CatchUp, definition.TargetMode, definition.PinnedConversationID, definition.IsEnabled, nextValue, now.UnixMilli(), now.UnixMilli()); err != nil {
			return store.ConstraintError(err)
		}
		if err = store.Audit(ctx, tx, actor, "routine.create", id, map[string]string{"name": definition.Name}); err != nil {
			return err
		}
		_, err = bus.Append(ctx, tx, "routine.updated", "", map[string]any{"routine_id": id, "revision": 1})
		return err
	})
	if err != nil {
		return domain.Routine{}, err
	}
	s.events.Notify()
	return s.Get(ctx, id)
}

// UpdateNow 直接修改已生效的例程，用于改名、暂停和恢复。
func (s *Routines) UpdateNow(ctx context.Context, id string, patch RoutinePatch, actor string) (domain.Routine, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return domain.Routine{}, err
	}
	if current.Revision != patch.Revision {
		return domain.Routine{}, routineChanged()
	}
	definition := current.RoutineDefinition
	if patch.Name.Set {
		definition.Name = patch.Name.Value
	}
	if patch.Cron.Set {
		definition.Cron = patch.Cron.Value
	}
	if patch.Prompt.Set {
		definition.Prompt = patch.Prompt.Value
	}
	if patch.IsEnabled.Set {
		if patch.IsEnabled.Value == nil {
			return domain.Routine{}, badInput("Routine enabled must be a boolean.")
		}
		definition.IsEnabled = *patch.IsEnabled.Value
	}
	input := RoutineInput{RoutineDefinition: definition, IsEnabled: &definition.IsEnabled, Revision: patch.Revision}
	now := s.now().UTC()
	normalized, err := normalizeRoutine(input, now)
	if err != nil {
		return domain.Routine{}, err
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		fresh, err := scanRoutine(tx.QueryRowContext(ctx, "SELECT "+routineColumns+" FROM routines WHERE id=?", id))
		if err != nil {
			return err
		}
		if fresh.Revision != patch.Revision {
			return routineChanged()
		}
		next, err := scheduler.Next(normalized.Cron, normalized.Timezone, now)
		if err != nil {
			return err
		}
		var nextValue any
		if normalized.IsEnabled {
			nextValue = next.UnixMilli()
		}
		if _, err = tx.ExecContext(ctx, `UPDATE routines SET name=?,cron=?,prompt=?,is_enabled=?,next_run_at=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, normalized.Name, normalized.Cron, normalized.Prompt, normalized.IsEnabled, nextValue, now.UnixMilli(), id, patch.Revision); err != nil {
			return err
		}
		if err = store.Audit(ctx, tx, actor, "routine.update", id, map[string]int64{"revision": patch.Revision + 1}); err != nil {
			return err
		}
		_, err = bus.Append(ctx, tx, "routine.updated", "", map[string]any{"routine_id": id, "revision": patch.Revision + 1})
		return err
	})
	if err != nil {
		return domain.Routine{}, err
	}
	s.events.Notify()
	return s.Get(ctx, id)
}

func routinePublicCode(err error) string {
	var public *domain.Error
	if errors.As(err, &public) {
		return public.Code
	}
	return "ROUTINE_DISPATCH_FAILED"
}

// expireRoutineTargetApprovals 在目标删除事务中使旧配置提案过期，不能在重建同名目标后误应用。
func expireRoutineTargetApprovals(ctx context.Context, tx *sql.Tx, target domain.RoutineTarget) error {
	result, err := tx.ExecContext(ctx, `UPDATE approvals SET status='expired',decided_at=? WHERE status='pending' AND id IN(SELECT approval_id FROM routine_changes WHERE (?!='' AND json_extract(definition_json,'$.target.bot_id')=?) OR (?!='' AND json_extract(definition_json,'$.target.group_id')=?))`, time.Now().UnixMilli(), target.BotID, target.BotID, target.GroupID, target.GroupID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return err
	}
	_, err = bus.Append(ctx, tx, "approval.resolved", "", map[string]string{"status": "expired"})
	return err
}
