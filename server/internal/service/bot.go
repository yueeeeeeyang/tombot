package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store"
)

type BotTemplate struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	RoleTitle string `json:"role_title"`
	Soul      string `json:"soul"`
}

func BotTemplates() []BotTemplate {
	return []BotTemplate{
		{"researcher", "研究员", "查证信息与整理资料", "You are a research colleague running on Tom. Prefer primary sources, distinguish evidence from inference, and cite sources. Keep private memory only when useful."},
		{"writer", "写作", "将材料整理成清晰文稿", "You are a writing colleague running on Tom. Preserve facts, use clear structure, adapt tone to the user's purpose, and place deliverables in your output directory."},
		{"assistant", "助理", "安排事务与跟进任务", "You are an assistant colleague running on Tom. Clarify goals when necessary, track commitments, and request approval before high-risk actions."},
	}
}

type BotInput struct {
	ID              string                  `json:"-"`
	Name            string                  `json:"name"`
	RoleTitle       string                  `json:"role_title"`
	Soul            string                  `json:"soul"`
	ModelID         *string                 `json:"model_id"`
	Template        string                  `json:"template"`
	AccentHue       *int                    `json:"accent_hue"`
	ToolPolicy      *domain.ToolPolicy      `json:"tool_policy"`
	MessagingPolicy *domain.MessagingPolicy `json:"messaging_policy"`
}
type BotPatch struct {
	Name            Field[string]                 `json:"name"`
	RoleTitle       Field[string]                 `json:"role_title"`
	Soul            Field[string]                 `json:"soul"`
	ModelID         Field[*string]                `json:"model_id"`
	AccentHue       Field[int]                    `json:"accent_hue"`
	IsArchived      Field[bool]                   `json:"is_archived"`
	ToolPolicy      Field[domain.ToolPolicy]      `json:"tool_policy"`
	MessagingPolicy Field[domain.MessagingPolicy] `json:"messaging_policy"`
}

// Bots 将身份、工作区和唯一单聊作为同一个业务实体创建；互斥锁覆盖文件与数据库变更。
type Bots struct {
	db          *sql.DB
	events      *bus.Bus
	workspace   string
	mu          sync.Mutex
	fileWriters map[string]int
}

func NewBots(db *sql.DB, events *bus.Bus, workspace string) *Bots {
	return &Bots{db: db, events: events, workspace: workspace, fileWriters: map[string]int{}}
}

// beginFileWrite 只在登记与释放时持锁，上传期间拒绝删除该 Bot，不阻塞其他 Bot 的管理操作。
func (s *Bots) beginFileWrite(ctx context.Context, id string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	s.fileWriters[id]++
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.fileWriters[id]--
		if s.fileWriters[id] == 0 {
			delete(s.fileWriters, id)
		}
	}, nil
}

func validateBot(bot *domain.Bot) error {
	bot.Name = strings.TrimSpace(bot.Name)
	bot.RoleTitle = strings.TrimSpace(bot.RoleTitle)
	bot.Soul = strings.TrimSpace(bot.Soul)
	if bot.Name == "" || len(bot.Name) > 120 || bot.RoleTitle == "" || len(bot.RoleTitle) > 300 || bot.Soul == "" || len(bot.Soul) > 32<<10 {
		return badInput("Bot name, role and instructions are required and must be within size limits.")
	}
	if bot.AccentHue < 0 || bot.AccentHue > 359 {
		return badInput("Accent hue must be between 0 and 359.")
	}
	switch bot.ToolPolicy.Approval {
	case "loose", "standard", "strict":
	default:
		return badInput("Invalid approval strictness.")
	}
	knownTools := map[string]bool{}
	for _, name := range ToolNames() {
		knownTools[name] = true
	}
	seen := map[string]bool{}
	for _, name := range bot.ToolPolicy.AllowedTools {
		if !knownTools[name] || seen[name] {
			return badInput("Tool names must be known and unique.")
		}
		seen[name] = true
	}
	return nil
}

const botColumns = `b.id,b.name,b.avatar_path,b.role_title,b.soul,b.model_id,b.tool_policy_json,b.messaging_policy_json,b.workspace_dir,b.accent_hue,b.is_archived,b.created_at,b.updated_at,c.id,
 CASE WHEN EXISTS(SELECT 1 FROM runs WHERE bot_id=b.id AND status='waiting_approval') THEN 'waiting_approval' WHEN EXISTS(SELECT 1 FROM runs WHERE bot_id=b.id AND status IN('running','queued')) THEN 'running' ELSE 'idle' END`
const botJoin = " FROM bots b JOIN conversations c ON c.bot_id=b.id AND c.kind='direct' AND c.source!='routine' "

func scanBot(row scanner) (domain.Bot, error) {
	var bot domain.Bot
	var tools, messaging string
	var created, updated int64
	err := row.Scan(&bot.ID, &bot.Name, &bot.AvatarPath, &bot.RoleTitle, &bot.Soul, &bot.ModelID, &tools, &messaging, &bot.WorkspaceDir, &bot.AccentHue, &bot.IsArchived, &created, &updated, &bot.ConversationID, &bot.Status)
	if err != nil {
		return bot, notFound(err)
	}
	if err = json.Unmarshal([]byte(tools), &bot.ToolPolicy); err != nil {
		return bot, err
	}
	if err = json.Unmarshal([]byte(messaging), &bot.MessagingPolicy); err != nil {
		return bot, err
	}
	bot.CreatedAt = time.UnixMilli(created).UTC()
	bot.UpdatedAt = time.UnixMilli(updated).UTC()
	return bot, nil
}
func (s *Bots) Get(ctx context.Context, id string) (domain.Bot, error) {
	return scanBot(s.db.QueryRowContext(ctx, "SELECT "+botColumns+botJoin+"WHERE b.id=?", id))
}

// ReadBot 在调用方事务内读取完整身份，供跨 Bot 投递再次校验消息策略与模型选择。
func ReadBot(ctx context.Context, tx *sql.Tx, id string) (domain.Bot, error) {
	return scanBot(tx.QueryRowContext(ctx, "SELECT "+botColumns+botJoin+"WHERE b.id=?", id))
}
func (s *Bots) List(ctx context.Context, cursor string, limit int, archived bool) ([]domain.Bot, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+botColumns+botJoin+"WHERE b.is_archived=? AND (?='' OR b.id<?) ORDER BY b.id DESC LIMIT ?", archived, cursor, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bots := []domain.Bot{}
	for rows.Next() {
		bot, err := scanBot(rows)
		if err != nil {
			return nil, err
		}
		bots = append(bots, bot)
	}
	return bots, rows.Err()
}
func (s *Bots) Create(ctx context.Context, input BotInput, actor string) (bot domain.Bot, result error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if input.Template != "" {
		found := false
		for _, template := range BotTemplates() {
			if template.ID == input.Template {
				found = true
				if input.Name == "" {
					input.Name = template.Name
				}
				if input.RoleTitle == "" {
					input.RoleTitle = template.RoleTitle
				}
				if input.Soul == "" {
					input.Soul = template.Soul
				}
			}
		}
		if !found {
			return bot, badInput("Unknown bot template.")
		}
	}
	now := time.Now().UTC()
	id := input.ID
	if id == "" {
		id = ids.New()
	}
	digest := sha256.Sum256([]byte(id))
	hue := (int(digest[0])*256 + int(digest[1])) % 360
	if input.AccentHue != nil {
		hue = *input.AccentHue
	}
	bot = domain.Bot{ID: id, Name: input.Name, RoleTitle: input.RoleTitle, Soul: input.Soul, ModelID: input.ModelID, AccentHue: hue, WorkspaceDir: filepath.ToSlash(filepath.Join("bots", id)), ConversationID: ids.New(), Status: "idle", CreatedAt: now, UpdatedAt: now, ToolPolicy: domain.ToolPolicy{AllowedTools: []string{"file_read", "file_list", "file_glob", "file_search", "memory_search", "memory_write", "memory_delete", "conversation_search", "conversation_read", "ask_user"}, Approval: "standard"}, MessagingPolicy: domain.MessagingPolicy{AcceptDM: true, AcceptGroupInvite: true}}
	if input.ToolPolicy != nil {
		bot.ToolPolicy = *input.ToolPolicy
	}
	if input.MessagingPolicy != nil {
		bot.MessagingPolicy = *input.MessagingPolicy
	}
	if err := validateBot(&bot); err != nil {
		return bot, err
	}
	root, err := os.OpenRoot(s.workspace)
	if err != nil {
		return bot, err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	directory := bot.WorkspaceDir
	if err := root.MkdirAll(filepath.Dir(directory), 0700); err != nil {
		return bot, err
	}
	if err := root.Mkdir(directory, 0700); err != nil {
		return bot, err
	}
	defer func() {
		if result != nil {
			result = errors.Join(result, root.RemoveAll(directory))
		}
	}()
	for _, part := range []string{"inbox", "out", "notes", ".tom/memory"} {
		if err := root.MkdirAll(filepath.Join(directory, part), 0700); err != nil {
			return bot, err
		}
	}
	tools, err := json.Marshal(bot.ToolPolicy)
	if err != nil {
		return bot, err
	}
	messaging, err := json.Marshal(bot.MessagingPolicy)
	if err != nil {
		return bot, err
	}
	result = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if input.AccentHue == nil {
			var err error
			bot.AccentHue, err = availableBotHue(ctx, tx, hue)
			if err != nil {
				return err
			}
		}
		if bot.ModelID == nil {
			var modelID string
			err := tx.QueryRowContext(ctx, "SELECT id FROM models WHERE is_default=1").Scan(&modelID)
			if err == nil {
				bot.ModelID = &modelID
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO bots(id,name,role_title,soul,model_id,tool_policy_json,messaging_policy_json,workspace_dir,accent_hue,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, bot.ID, bot.Name, bot.RoleTitle, bot.Soul, bot.ModelID, string(tools), string(messaging), bot.WorkspaceDir, bot.AccentHue, now.UnixMilli(), now.UnixMilli()); err != nil {
			return store.ConstraintError(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversations(id,kind,bot_id,title,created_at,updated_at) VALUES(?,'direct',?,?,?,?)`, bot.ConversationID, bot.ID, bot.Name, now.UnixMilli(), now.UnixMilli()); err != nil {
			return err
		}
		if err := store.Audit(ctx, tx, actor, "bot.create", bot.ID, map[string]string{"conversation_id": bot.ConversationID}); err != nil {
			return err
		}
		_, err := bus.Append(ctx, tx, "conversation.updated", bot.ConversationID, map[string]any{"conversation_id": bot.ConversationID, "bot_id": bot.ID, "title": bot.Name, "unread_count": 0, "bot_status": "idle"})
		return err
	})
	if result == nil {
		s.events.Notify()
	}
	return bot, result
}
func (s *Bots) Update(ctx context.Context, id string, patch BotPatch, actor string) (bot domain.Bot, result error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		bot, err = scanBot(tx.QueryRowContext(ctx, "SELECT "+botColumns+botJoin+"WHERE b.id=?", id))
		if err != nil {
			return err
		}
		if bot.Status != "idle" {
			return domain.NewError(409, "BOT_BUSY", "Wait for the bot to finish before changing its configuration.")
		}
		if err := requireBotGroupIdle(ctx, tx, id); err != nil {
			return err
		}
		if patch.Name.Set {
			bot.Name = patch.Name.Value
		}
		if patch.RoleTitle.Set {
			bot.RoleTitle = patch.RoleTitle.Value
		}
		if patch.Soul.Set {
			bot.Soul = patch.Soul.Value
		}
		if patch.ModelID.Set {
			bot.ModelID = patch.ModelID.Value
		}
		if patch.AccentHue.Set {
			bot.AccentHue = patch.AccentHue.Value
		}
		if patch.IsArchived.Set {
			bot.IsArchived = patch.IsArchived.Value
		}
		if patch.ToolPolicy.Set {
			bot.ToolPolicy = patch.ToolPolicy.Value
		}
		if patch.MessagingPolicy.Set {
			bot.MessagingPolicy = patch.MessagingPolicy.Value
		}
		if err = validateBot(&bot); err != nil {
			return err
		}
		tools, err := json.Marshal(bot.ToolPolicy)
		if err != nil {
			return err
		}
		messaging, err := json.Marshal(bot.MessagingPolicy)
		if err != nil {
			return err
		}
		bot.UpdatedAt = time.Now().UTC()
		if _, err = tx.ExecContext(ctx, `UPDATE bots SET name=?,role_title=?,soul=?,model_id=?,accent_hue=?,is_archived=?,tool_policy_json=?,messaging_policy_json=?,updated_at=? WHERE id=?`, bot.Name, bot.RoleTitle, bot.Soul, bot.ModelID, bot.AccentHue, bot.IsArchived, string(tools), string(messaging), bot.UpdatedAt.UnixMilli(), id); err != nil {
			return store.ConstraintError(err)
		}
		if _, err = tx.ExecContext(ctx, "UPDATE conversations SET title=?,is_archived=?,updated_at=? WHERE id=?", bot.Name, bot.IsArchived, bot.UpdatedAt.UnixMilli(), bot.ConversationID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE conversations SET title=(SELECT name FROM bots WHERE id=conversations.initiator_bot_id)||' ↔ '||(SELECT name FROM bots WHERE id=conversations.bot_id),updated_at=? WHERE kind='bot_dm' AND (bot_id=? OR initiator_bot_id=?)`, bot.UpdatedAt.UnixMilli(), bot.ID, bot.ID); err != nil {
			return err
		}
		if err = store.Audit(ctx, tx, actor, "bot.update", id, map[string]string{}); err != nil {
			return err
		}
		if _, err = bus.Append(ctx, tx, "bot.updated", "", map[string]string{"bot_id": id}); err != nil {
			return err
		}
		_, err = bus.Append(ctx, tx, "conversation.updated", bot.ConversationID, map[string]any{"conversation_id": bot.ConversationID, "title": bot.Name, "is_archived": bot.IsArchived})
		return err
	})
	if result == nil {
		s.events.Notify()
	}
	return bot, result
}

// Delete 拒绝正在执行的 Bot；删除转录与索引，默认保留工作区供用户回收。
func (s *Bots) Delete(ctx context.Context, id, confirmation, actor string, purge bool) (result error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fileWriters[id] > 0 {
		return domain.NewError(409, "BOT_FILES_BUSY", "Wait for this bot's file uploads and edits to finish before deleting it.")
	}
	bot, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if confirmation != bot.Name {
		return domain.NewError(400, "BOT_CONFIRMATION_REQUIRED", "Confirm the exact bot name before deletion.")
	}
	root, err := os.OpenRoot(s.workspace)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	directory := filepath.Join("bots", bot.ID)
	staged := ""
	defer func() {
		if result != nil && staged != "" {
			result = errors.Join(result, root.Rename(staged, directory))
		}
	}()
	result = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if err := requireBotGroupIdle(ctx, tx, id); err != nil {
			return err
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE (bot_id=? OR conversation_id IN(SELECT id FROM conversations WHERE bot_id=? OR initiator_bot_id=?)) AND status IN('queued','running','waiting_approval')`, id, id, id).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return domain.NewError(409, "BOT_BUSY", "Cancel active runs before deleting the bot.")
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM terminal_sessions WHERE bot_id=? AND status IN('starting','running','closing','exited')`, id).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return domain.NewError(409, "BOT_TERMINALS_OPEN", "Close the bot's terminal sessions before deleting the bot.")
		}
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM browser_tabs WHERE bot_id=?", id).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return domain.NewError(409, "BOT_BROWSER_TABS_OPEN", "Close the bot's browser tabs before deleting the bot.")
		}
		if purge {
			staged = filepath.Join("bots", ".deleted-"+bot.ID)
			if err = root.Rename(directory, staged); err != nil && !os.IsNotExist(err) {
				staged = ""
				return err
			}
			if os.IsNotExist(err) {
				staged = ""
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE groups SET revision=revision+1,updated_at=? WHERE id IN(SELECT group_id FROM group_members WHERE bot_id=?)", time.Now().UnixMilli(), id); err != nil {
			return err
		}
		if _, err = bus.Append(ctx, tx, "group.updated", "", map[string]string{"removed_bot_id": id}); err != nil {
			return err
		}
		// 同时删除私信双方共享的会话及其执行记录；群组历史保留发送者标识。
		if err = expireRoutineTargetApprovals(ctx, tx, domain.RoutineTarget{BotID: id}); err != nil {
			return err
		}
		conversations := `SELECT id FROM conversations WHERE bot_id=? OR initiator_bot_id=?`
		runs := `SELECT id FROM runs WHERE bot_id=? OR conversation_id IN (` + conversations + `)`
		for _, statement := range []string{`DELETE FROM approvals WHERE run_id IN (` + runs + `)`, `DELETE FROM tool_calls WHERE run_id IN (` + runs + `)`, `DELETE FROM runs WHERE id IN (` + runs + `)`} {
			if _, err := tx.ExecContext(ctx, statement, id, id, id); err != nil {
				return err
			}
		}
		for _, statement := range []string{`DELETE FROM messages WHERE conversation_id IN (` + conversations + `)`, `DELETE FROM turns WHERE conversation_id IN (` + conversations + `)`} {
			if _, err := tx.ExecContext(ctx, statement, id, id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE id IN (`+conversations+`)`, id, id); err != nil {
			return err
		}
		for _, statement := range []string{`DELETE FROM routine_runs WHERE routine_id IN(SELECT id FROM routines WHERE bot_id=?)`, `DELETE FROM routines WHERE bot_id=?`, `DELETE FROM group_members WHERE bot_id=?`, `DELETE FROM policy_rules WHERE bot_id=?`, `DELETE FROM memory_index WHERE bot_id=?`, `DELETE FROM memory_fts_ascii WHERE bot_id=?`, `DELETE FROM memory_fts_cjk WHERE bot_id=?`} {
			if _, err := tx.ExecContext(ctx, statement, id); err != nil {
				return err
			}
		}
		if err := store.RequireChanged(tx.ExecContext(ctx, "DELETE FROM bots WHERE id=?", id)); err != nil {
			return err
		}
		if err := store.Audit(ctx, tx, actor, "bot.delete", id, map[string]bool{"purge_workspace": purge}); err != nil {
			return err
		}
		_, err := bus.Append(ctx, tx, "conversation.updated", bot.ConversationID, map[string]any{"conversation_id": bot.ConversationID, "deleted": true})
		return err
	})
	if result != nil {
		return result
	}
	s.events.Notify()
	if staged != "" {
		if err = root.RemoveAll(staged); err != nil {
			staged = ""
			return fmt.Errorf("Bot 已删除，但待清理目录移除失败: %w", err)
		}
		staged = ""
	}
	return nil
}

// requireBotGroupIdle 覆盖规划前和成员之间没有 Run 的空隙，群回合快照期间禁止修改或删除成员。
func requireBotGroupIdle(ctx context.Context, tx *sql.Tx, id string) error {
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM group_members m JOIN conversations c ON c.group_id=m.group_id AND c.kind='group' JOIN turns t ON t.conversation_id=c.id WHERE m.bot_id=? AND t.status IN('queued','running','waiting_approval'))`, id).Scan(&active); err != nil {
		return err
	}
	if active {
		return domain.NewError(409, "BOT_BUSY", "Finish or cancel the bot's active group turn before changing or deleting it.")
	}
	return nil
}

// ToolNames 返回设置页可配置的工具标识，客户端不再维护第二份白名单。
func ToolNames() []string {
	names := []string{"file_read", "file_write", "file_edit", "file_list", "file_glob", "file_search", "computer_shell", "terminal_session", "browser", "message_bot", "post_to_group", "group_members", "delegate_task", "memory_search", "memory_write", "memory_delete", "conversation_search", "conversation_read", "ask_user"}
	sort.Strings(names)
	return names
}

// availableBotHue 在活动 Bot 色环的最大空隙中间选色。首个 Bot 或全部 360 色已用时使用身份哈希。
func availableBotHue(ctx context.Context, tx *sql.Tx, initial int) (int, error) {
	rows, err := tx.QueryContext(ctx, "SELECT DISTINCT accent_hue FROM bots WHERE is_archived=0 ORDER BY accent_hue")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	first, previous := -1, -1
	longest, start := 0, 0
	for rows.Next() {
		var hue int
		if err := rows.Scan(&hue); err != nil {
			return 0, err
		}
		if hue < 0 || hue >= 360 {
			return 0, fmt.Errorf("现有 Bot 身份色超出范围")
		}
		if first < 0 {
			first = hue
		} else if hue-previous > longest {
			longest, start = hue-previous, previous
		}
		previous = hue
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if first < 0 {
		return initial, nil
	}
	if gap := first + 360 - previous; gap > longest {
		longest, start = gap, previous
	}
	if longest < 2 {
		return initial, nil
	}
	return (start + longest/2) % 360, nil
}
