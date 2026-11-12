package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store"
)

type Groups struct {
	db     *sql.DB
	events *bus.Bus
}

func NewGroups(db *sql.DB, events *bus.Bus) *Groups { return &Groups{db: db, events: events} }

type GroupInput struct {
	Name         string   `json:"name"`
	Purpose      string   `json:"purpose"`
	Members      []string `json:"members"`
	MaxCostMicro *int64   `json:"max_cost_micro"`
}
type GroupPatch struct {
	Revision     int64         `json:"revision"`
	Name         Field[string] `json:"name"`
	Purpose      Field[string] `json:"purpose"`
	MaxCostMicro Field[int64]  `json:"max_cost_micro"`
}
type GroupMembersInput struct {
	Revision int64    `json:"revision"`
	BotIDs   []string `json:"bot_ids"`
}

const groupColumns = `g.id,g.name,g.purpose,g.avatar_presets_json,g.max_cost_micro,g.revision,c.id,g.created_at,g.updated_at`
const groupJoin = ` FROM groups g JOIN conversations c ON c.group_id=g.id AND c.kind='group' AND c.source!='routine' `

func scanGroup(row scanner) (group domain.Group, err error) {
	var created, updated int64
	var budget sql.NullInt64
	var avatars string
	err = row.Scan(&group.ID, &group.Name, &group.Purpose, &avatars, &budget, &group.Revision, &group.ConversationID, &created, &updated)
	if err == nil {
		err = json.Unmarshal([]byte(avatars), &group.AvatarPresets)
	}
	group.MaxCostMicro = domain.DefaultGroupBudgetMicro
	if budget.Valid {
		group.MaxCostMicro = budget.Int64
	}
	group.CreatedAt, group.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	group.Members = []domain.GroupMember{}
	return group, notFound(err)
}

// ReadGroup 在同一事务内读配置与有界成员，供管理和回合快照共同使用。
func ReadGroup(ctx context.Context, tx *sql.Tx, id string) (domain.Group, error) {
	group, err := scanGroup(tx.QueryRowContext(ctx, "SELECT "+groupColumns+groupJoin+"WHERE g.id=?", id))
	if err != nil {
		return group, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT b.id,b.name,b.role_title,b.accent_hue,b.is_archived FROM group_members m JOIN bots b ON b.id=m.bot_id WHERE m.group_id=? ORDER BY m.joined_at,b.id LIMIT ?`, id, domain.MaxGroupMembers+1)
	if err != nil {
		return group, err
	}
	defer rows.Close()
	for rows.Next() {
		var member domain.GroupMember
		if err = rows.Scan(&member.BotID, &member.Name, &member.RoleTitle, &member.AccentHue, &member.IsArchived); err != nil {
			return group, err
		}
		group.Members = append(group.Members, member)
	}
	if len(group.Members) > domain.MaxGroupMembers {
		return group, domain.NewError(409, "GROUP_CAPACITY", "The group exceeds the member limit.")
	}
	return group, rows.Err()
}

func (s *Groups) Get(ctx context.Context, id string) (group domain.Group, err error) {
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error { var readErr error; group, readErr = ReadGroup(ctx, tx, id); return readErr })
	return
}

// List 只返回分页配置；详情与成员通过单群接口读取，避免逐群查询。
func (s *Groups) List(ctx context.Context, cursor string, limit int) ([]domain.Group, error) {
	if limit < 1 || limit > 200 {
		return nil, badInput("Limit must be between 1 and 200.")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+groupColumns+groupJoin+"WHERE (?='' OR g.id<?) ORDER BY g.id DESC LIMIT ?", cursor, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := []domain.Group{}
	for rows.Next() {
		group, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func validateGroup(group *domain.Group) error {
	group.Name = strings.TrimSpace(group.Name)
	group.Purpose = strings.TrimSpace(group.Purpose)
	if group.Name == "" || len(group.Name) > 200 || len(group.Purpose) > 8192 || group.MaxCostMicro < 1 || group.MaxCostMicro > 1_000_000_000 {
		return badInput("Invalid group name, purpose, speaker limit, or budget.")
	}
	return nil
}

func validateMemberIDs(ids []string) error {
	if len(ids) == 0 || len(ids) > domain.MaxGroupMembers {
		return badInput("A group must contain 1 to 32 unique members.")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || len(id) > 128 || seen[id] {
			return badInput("Member identifiers must be nonempty and unique.")
		}
		seen[id] = true
	}
	return nil
}

func addGroupMembers(ctx context.Context, tx *sql.Tx, groupID string, ids []string) error {
	if err := validateMemberIDs(ids); err != nil {
		return err
	}
	for _, id := range ids {
		var archived, accepts bool
		if err := tx.QueryRowContext(ctx, `SELECT is_archived,COALESCE(json_extract(messaging_policy_json,'$.accept_group_invite'),0) FROM bots WHERE id=?`, id).Scan(&archived, &accepts); err != nil {
			return notFound(err)
		}
		if archived || !accepts {
			return domain.NewError(403, "GROUP_INVITE_DENIED", "The bot does not accept group invitations.")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_members(group_id,bot_id,joined_at) VALUES(?,?,?) ON CONFLICT(group_id,bot_id) DO NOTHING`, groupID, id, time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM group_members WHERE group_id=?", groupID).Scan(&count); err != nil {
		return err
	}
	if count > domain.MaxGroupMembers {
		return domain.NewError(409, "GROUP_CAPACITY", "A group supports at most 32 members.")
	}
	return nil
}

func (s *Groups) Create(ctx context.Context, input GroupInput, actor string) (group domain.Group, err error) {
	if err = validateMemberIDs(input.Members); err != nil {
		return
	}
	now := time.Now().UTC()
	group = domain.Group{ID: ids.New(), ConversationID: ids.New(), Name: input.Name, Purpose: input.Purpose, MaxCostMicro: domain.DefaultGroupBudgetMicro, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if input.MaxCostMicro != nil {
		group.MaxCostMicro = *input.MaxCostMicro
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO groups(id,name,purpose,max_cost_micro,created_at,updated_at) VALUES(?,?,?,?,?,?)`, group.ID, group.Name, group.Purpose, group.MaxCostMicro, now.UnixMilli(), now.UnixMilli()); err != nil {
			return store.ConstraintError(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversations(id,kind,source,group_id,title,created_at,updated_at) VALUES(?,'group','user',?,?,?,?)`, group.ConversationID, group.ID, group.Name, now.UnixMilli(), now.UnixMilli()); err != nil {
			return err
		}
		if err := addGroupMembers(ctx, tx, group.ID, input.Members); err != nil {
			return err
		}
		avatars := []string{}
		for _, id := range input.Members[:min(4, len(input.Members))] {
			var preset string
			if err := tx.QueryRowContext(ctx, "SELECT CASE WHEN custom_avatar!='' THEN custom_avatar WHEN avatar_preset='' THEN 'bombay' ELSE avatar_preset END FROM bots WHERE id=?", id).Scan(&preset); err != nil {
				return err
			}
			avatars = append(avatars, preset)
		}
		encoded, err := json.Marshal(avatars)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE groups SET avatar_presets_json=? WHERE id=?", string(encoded), group.ID); err != nil {
			return err
		}
		var readErr error
		group, readErr = ReadGroup(ctx, tx, group.ID)
		if readErr != nil {
			return readErr
		}
		if err := validateGroup(&group); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE groups SET name=?,purpose=? WHERE id=?", group.Name, group.Purpose, group.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE conversations SET title=? WHERE id=?", group.Name, group.ConversationID); err != nil {
			return err
		}
		return groupChanged(ctx, tx, group, actor, "group.create")
	})
	if err == nil {
		s.events.Notify()
	}
	return
}

func groupChanged(ctx context.Context, tx *sql.Tx, group domain.Group, actor, action string) error {
	if err := store.Audit(ctx, tx, actor, action, group.ID, map[string]any{"revision": group.Revision, "members": len(group.Members)}); err != nil {
		return err
	}
	if _, err := bus.Append(ctx, tx, "group.updated", group.ConversationID, map[string]any{"group_id": group.ID, "revision": group.Revision}); err != nil {
		return err
	}
	return AppendConversationEvent(ctx, tx, group.ConversationID)
}

func groupEditable(ctx context.Context, tx *sql.Tx, group domain.Group, revision int64) error {
	if revision != group.Revision {
		return domain.NewError(409, "GROUP_CHANGED", "The group configuration changed. Reload before editing.")
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM turns t JOIN conversations c ON c.id=t.conversation_id WHERE c.group_id=? AND t.status IN('queued','running','waiting_approval'))`, group.ID).Scan(&active); err != nil {
		return err
	}
	if active {
		return domain.NewError(409, "TURN_IN_PROGRESS", "Cancel or finish the active group turn before editing.")
	}
	return nil
}

func (s *Groups) Update(ctx context.Context, id string, patch GroupPatch, actor string) (group domain.Group, err error) {
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		group, err = ReadGroup(ctx, tx, id)
		if err != nil {
			return err
		}
		if err = groupEditable(ctx, tx, group, patch.Revision); err != nil {
			return err
		}
		if patch.Name.Set {
			group.Name = patch.Name.Value
		}
		if patch.Purpose.Set {
			group.Purpose = patch.Purpose.Value
		}
		if patch.MaxCostMicro.Set {
			group.MaxCostMicro = patch.MaxCostMicro.Value
		}
		if err = validateGroup(&group); err != nil {
			return err
		}
		group.Revision++
		group.UpdatedAt = time.Now().UTC()
		if _, err = tx.ExecContext(ctx, `UPDATE groups SET name=?,purpose=?,max_cost_micro=?,revision=?,updated_at=? WHERE id=?`, group.Name, group.Purpose, group.MaxCostMicro, group.Revision, group.UpdatedAt.UnixMilli(), id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE conversations SET title=?,updated_at=? WHERE id=?", group.Name, group.UpdatedAt.UnixMilli(), group.ConversationID); err != nil {
			return err
		}
		return groupChanged(ctx, tx, group, actor, "group.update")
	})
	if err == nil {
		s.events.Notify()
	}
	return
}

func (s *Groups) ChangeMembers(ctx context.Context, id string, input GroupMembersInput, remove, actor string) (group domain.Group, err error) {
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		group, err = ReadGroup(ctx, tx, id)
		if err != nil {
			return err
		}
		if err = groupEditable(ctx, tx, group, input.Revision); err != nil {
			return err
		}
		if remove != "" {
			if len(group.Members) <= 1 {
				return domain.NewError(409, "GROUP_LAST_MEMBER", "A group must retain at least one member.")
			}
			if err = store.RequireChanged(tx.ExecContext(ctx, "DELETE FROM group_members WHERE group_id=? AND bot_id=?", id, remove)); err != nil {
				return err
			}
		} else if err = addGroupMembers(ctx, tx, id, input.BotIDs); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE groups SET revision=revision+1,updated_at=? WHERE id=?", time.Now().UnixMilli(), id); err != nil {
			return err
		}
		group, err = ReadGroup(ctx, tx, id)
		if err != nil {
			return err
		}
		return groupChanged(ctx, tx, group, actor, "group.members.update")
	})
	if err == nil {
		s.events.Notify()
	}
	return
}

// Delete 在确认版本且无活动回合后删除该群及转录，成员身份和工作区保持独立。
func (s *Groups) Delete(ctx context.Context, id string, revision int64, actor string) error {
	err := store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		group, err := ReadGroup(ctx, tx, id)
		if err != nil {
			return err
		}
		if err = groupEditable(ctx, tx, group, revision); err != nil {
			return err
		}
		if err = expireRoutineTargetApprovals(ctx, tx, domain.RoutineTarget{GroupID: id}); err != nil {
			return err
		}
		conversations := `SELECT id FROM conversations WHERE group_id=?`
		for _, statement := range []string{`DELETE FROM tool_calls WHERE run_id IN(SELECT id FROM runs WHERE conversation_id IN (` + conversations + `))`, `DELETE FROM runs WHERE conversation_id IN (` + conversations + `)`, `DELETE FROM messages WHERE conversation_id IN (` + conversations + `)`, `DELETE FROM turns WHERE conversation_id IN (` + conversations + `)`, `DELETE FROM conversations WHERE group_id=?`} {
			if _, err = tx.ExecContext(ctx, statement, id); err != nil {
				return err
			}
		}
		for _, statement := range []string{`DELETE FROM routine_runs WHERE routine_id IN(SELECT id FROM routines WHERE group_id=?)`, `DELETE FROM routines WHERE group_id=?`, `DELETE FROM group_members WHERE group_id=?`, `DELETE FROM groups WHERE id=?`} {
			if _, err = tx.ExecContext(ctx, statement, id); err != nil {
				return err
			}
		}
		if err = store.Audit(ctx, tx, actor, "group.delete", id, map[string]string{}); err != nil {
			return err
		}
		if _, err = bus.Append(ctx, tx, "group.updated", group.ConversationID, map[string]any{"group_id": id, "deleted": true}); err != nil {
			return err
		}
		_, err = bus.Append(ctx, tx, "conversation.updated", group.ConversationID, map[string]any{"conversation_id": group.ConversationID, "deleted": true})
		return err
	})
	if err == nil {
		s.events.Notify()
	}
	return err
}
