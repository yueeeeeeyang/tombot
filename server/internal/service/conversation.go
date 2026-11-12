package service

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/store"
)

type Conversations struct {
	db     *sql.DB
	events *bus.Bus
}

func NewConversations(db *sql.DB, events *bus.Bus) *Conversations {
	return &Conversations{db: db, events: events}
}

type ConversationPage struct {
	Items      []domain.Conversation `json:"items"`
	NextCursor string                `json:"next_cursor,omitempty"`
}

type ConversationSnapshot struct {
	Conversation     domain.Conversation `json:"conversation"`
	Messages         []domain.Message    `json:"messages"`
	EventCursor      int64               `json:"event_cursor"`
	ActiveTurnID     *string             `json:"active_turn_id"`
	ActiveTurnStatus *string             `json:"active_turn_status,omitempty"`
	ActiveRuns       []ActiveRun         `json:"active_runs"`
}

type ActiveRun struct {
	ModelName   string               `json:"model_name"`
	CurrentTool *domain.ToolActivity `json:"current_tool,omitempty"`
	ID          string               `json:"id"`
	BotID       string               `json:"bot_id"`
	Status      string               `json:"status"`
	PlanRole    string               `json:"plan_role"`
}

// Snapshot 在同一数据库快照内读取消息和事件游标，避免客户端首次连接重复应用增量。
func (s *Conversations) Snapshot(ctx context.Context, id string) (snapshot ConversationSnapshot, result error) {
	result = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		snapshot.Conversation, err = scanConversation(tx.QueryRowContext(ctx, "SELECT "+conversationColumns+" FROM conversations c WHERE c.id=?", id))
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,conversation_id,turn_id,run_id,seq,sender_kind,sender_bot_id,blocks_json,status,token_in,token_out,created_at FROM messages WHERE conversation_id=? ORDER BY seq DESC LIMIT 50`, id)
		if err != nil {
			return err
		}
		snapshot.Messages, err = readMessages(rows)
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(id),0) FROM events").Scan(&snapshot.EventCursor); err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, "SELECT id,status FROM turns WHERE conversation_id=? AND status IN('queued','running','waiting_approval') LIMIT 1", id).Scan(&snapshot.ActiveTurnID, &snapshot.ActiveTurnStatus)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		snapshot.ActiveRuns = []ActiveRun{}
		rows, err = tx.QueryContext(ctx, `SELECT r.id,r.bot_id,r.status,COALESCE(r.plan_role,'solo'),COALESCE(m.display_name,r.model_id) FROM runs r LEFT JOIN models m ON m.id=r.model_id WHERE r.turn_id=? AND r.status IN('queued','running','waiting_approval') ORDER BY r.created_at,r.id LIMIT ?`, snapshot.ActiveTurnID, domain.MaxGroupMembers+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var run ActiveRun
			if err = rows.Scan(&run.ID, &run.BotID, &run.Status, &run.PlanRole, &run.ModelName); err != nil {
				return err
			}
			snapshot.ActiveRuns = append(snapshot.ActiveRuns, run)
		}
		if len(snapshot.ActiveRuns) > domain.MaxGroupMembers {
			return domain.NewError(500, "RUN_CAPACITY", "The conversation exceeds the active run limit.")
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		return attachToolActivities(ctx, tx, snapshot.Messages, snapshot.ActiveRuns)
	})
	return snapshot, result
}

type conversationCursor struct {
	UpdatedAt int64  `json:"updated_at"`
	ID        string `json:"id"`
}

const conversationColumns = `c.id,c.kind,c.source,c.bot_id,c.group_id,c.initiator_bot_id,c.title,c.summary,c.last_read_seq,c.is_archived,c.created_at,c.updated_at,
 COALESCE((SELECT blocks_json FROM messages WHERE conversation_id=c.id ORDER BY seq DESC LIMIT 1),'[]'),
 (SELECT COUNT(*) FROM messages WHERE conversation_id=c.id AND seq>c.last_read_seq AND sender_kind!='user'),
 (SELECT COUNT(*) FROM messages m, json_each(m.blocks_json) block
  WHERE m.conversation_id=c.id AND m.seq>c.last_read_seq AND m.sender_kind!='user' AND instr(m.blocks_json,'"artifact"')>0
   AND ((json_extract(block.value,'$.kind')='artifact' AND json_type(block.value,'$.artifact.path')='text')
     OR (json_extract(block.value,'$.kind')='tool_result' AND json_type(block.value,'$.tool_result.artifact.path')='text'))),
 EXISTS(SELECT 1 FROM turns WHERE conversation_id=c.id AND status IN ('queued','running')),
 CASE WHEN EXISTS(SELECT 1 FROM runs WHERE conversation_id=c.id AND status='waiting_approval') OR EXISTS(SELECT 1 FROM group_members gm JOIN runs r ON r.bot_id=gm.bot_id WHERE gm.group_id=c.group_id AND r.status='waiting_approval') THEN 'waiting_approval' WHEN EXISTS(SELECT 1 FROM turns WHERE conversation_id=c.id AND status IN('running','queued','waiting_approval')) OR EXISTS(SELECT 1 FROM group_members gm JOIN runs r ON r.bot_id=gm.bot_id WHERE gm.group_id=c.group_id AND r.status IN('running','queued')) THEN 'running' ELSE 'idle' END,
 COALESCE((SELECT accent_hue FROM bots WHERE id=c.bot_id),0),
 CASE WHEN c.kind='bot_dm' THEN COALESCE((SELECT json_group_array(json_object('bot_id',b.id,'name',b.name,'role_title',b.role_title,'accent_hue',b.accent_hue,'is_archived',json(CASE WHEN b.is_archived THEN 'true' ELSE 'false' END))) FROM bots b WHERE b.id IN(c.bot_id,c.initiator_bot_id)),'[]') ELSE COALESCE((SELECT json_group_array(json_object('bot_id',b.id,'name',b.name,'role_title',b.role_title,'accent_hue',b.accent_hue,'is_archived',json(CASE WHEN b.is_archived THEN 'true' ELSE 'false' END))) FROM group_members m JOIN bots b ON b.id=m.bot_id WHERE m.group_id=c.group_id),'[]') END`

func scanConversation(row scanner) (domain.Conversation, error) {
	var c domain.Conversation
	var created, updated int64
	var blocksJSON string
	var membersJSON string
	err := row.Scan(&c.ID, &c.Kind, &c.Source, &c.BotID, &c.GroupID, &c.InitiatorBotID, &c.Title, &c.Summary, &c.LastReadSeq, &c.IsArchived, &created, &updated, &blocksJSON, &c.UnreadCount, &c.UnreadArtifacts, &c.HasActiveTurn, &c.BotStatus, &c.AccentHue, &membersJSON)
	if err != nil {
		return c, notFound(err)
	}
	if err = json.Unmarshal([]byte(membersJSON), &c.Members); err != nil {
		return c, err
	}
	c.CreatedAt = time.UnixMilli(created).UTC()
	c.UpdatedAt = time.UnixMilli(updated).UTC()
	var blocks []domain.MessageBlock
	if err = json.Unmarshal([]byte(blocksJSON), &blocks); err != nil {
		return c, err
	}
	var preview strings.Builder
	for _, block := range blocks {
		if block.Kind == "text" || block.Kind == "note" {
			preview.WriteString(block.Text)
		}
	}
	characters := []rune(preview.String())
	c.LastMessagePreview = string(characters[:min(len(characters), 160)])
	return c, nil
}
func (s *Conversations) Get(ctx context.Context, id string) (domain.Conversation, error) {
	return scanConversation(s.db.QueryRowContext(ctx, "SELECT "+conversationColumns+" FROM conversations c WHERE c.id=?", id))
}
func (s *Conversations) Direct(ctx context.Context, botID string) (domain.Conversation, error) {
	return scanConversation(s.db.QueryRowContext(ctx, "SELECT "+conversationColumns+" FROM conversations c WHERE c.kind='direct' AND c.source!='routine' AND c.bot_id=?", botID))
}
func (s *Conversations) List(ctx context.Context, kind, cursor string, limit int) (ConversationPage, error) {
	page := ConversationPage{Items: []domain.Conversation{}}
	if limit < 1 || limit > 200 {
		return page, badInput("Limit must be between 1 and 200.")
	}
	if kind != "" && kind != "direct" && kind != "group" && kind != "bot_dm" {
		return page, badInput("Unknown conversation kind.")
	}
	var after conversationCursor
	if cursor != "" {
		encoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(encoded, &after) != nil || after.ID == "" || after.UpdatedAt < 0 {
			return page, badInput("Invalid conversation cursor.")
		}
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+conversationColumns+` FROM conversations c WHERE c.is_archived=0 AND (?='' OR c.kind=?) AND (?='' OR c.updated_at<? OR (c.updated_at=? AND c.id<?)) ORDER BY c.updated_at DESC,c.id DESC LIMIT ?`, kind, kind, cursor, after.UpdatedAt, after.UpdatedAt, after.ID, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, c)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[len(page.Items)-1]
		encoded, err := json.Marshal(conversationCursor{last.UpdatedAt.UnixMilli(), last.ID})
		if err != nil {
			return page, err
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return page, nil
}
func (s *Conversations) SetReply(ctx context.Context, messageID, replyToID string) error {
	if messageID == "" || replyToID == "" || messageID == replyToID {
		return badInput("Reply target must be another message.")
	}
	return store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		var conversationID string
		if err := tx.QueryRowContext(ctx, `SELECT conversation_id FROM messages WHERE id=?`, messageID).Scan(&conversationID); err != nil {
			return notFound(err)
		}
		var same int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE id=? AND conversation_id=?`, replyToID, conversationID).Scan(&same); err != nil {
			return err
		}
		if same != 1 {
			return domain.NewError(400, "MESSAGE_INVALID", "The reply target is not in this conversation.")
		}
		_, err := tx.ExecContext(ctx, `UPDATE messages SET reply_to_id=? WHERE id=?`, replyToID, messageID)
		return err
	})
}

func (s *Conversations) ToggleReaction(ctx context.Context, messageID, emoji string) error {
	if messageID == "" || emoji == "" || len(emoji) > 16 {
		return badInput("Choose a reaction emoji.")
	}
	return store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE id=?`, messageID).Scan(&exists); err != nil {
			return err
		}
		if exists != 1 {
			return domain.NewError(404, "NOT_FOUND", "The message does not exist.")
		}
		var count int
		err := tx.QueryRowContext(ctx, `SELECT count FROM message_reactions WHERE message_id=? AND emoji=?`, messageID, emoji).Scan(&count)
		if err == sql.ErrNoRows {
			_, err = tx.ExecContext(ctx, `INSERT INTO message_reactions(message_id,emoji,count) VALUES(?,?,1)`, messageID, emoji)
			return err
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM message_reactions WHERE message_id=? AND emoji=?`, messageID, emoji)
		return err
	})
}

// RunBelongsToTurn 核对通知是否来自当前回合的有界子任务，避免把其他会话混入事件流。
func (s *Conversations) RunBelongsToTurn(ctx context.Context, runID, turnID string) (bool, error) {
	var related bool
	err := s.db.QueryRowContext(ctx, `WITH RECURSIVE chain(id,parent_run_id,turn_id,depth) AS (SELECT id,parent_run_id,turn_id,0 FROM runs WHERE id=? UNION ALL SELECT r.id,r.parent_run_id,r.turn_id,chain.depth+1 FROM runs r JOIN chain ON r.id=chain.parent_run_id WHERE chain.depth<8) SELECT EXISTS(SELECT 1 FROM chain WHERE turn_id=?)`, runID, turnID).Scan(&related)
	return related, err
}

func (s *Conversations) Messages(ctx context.Context, id string, before int64, limit int) ([]domain.Message, error) {
	if limit < 1 || limit > 200 || before < 0 {
		return nil, badInput("Invalid message page bounds.")
	}
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	var messages []domain.Message
	err := store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id,conversation_id,turn_id,run_id,seq,sender_kind,sender_bot_id,blocks_json,status,token_in,token_out,created_at FROM messages WHERE conversation_id=? AND (?=0 OR seq<?) ORDER BY seq DESC LIMIT ?`, id, before, before, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		messages, err = readMessages(rows)
		if err != nil {
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
		return attachToolActivities(ctx, tx, messages, nil)
	})
	return messages, err
}

func readMessages(rows *sql.Rows) ([]domain.Message, error) {
	messages := []domain.Message{}
	for rows.Next() {
		var m domain.Message
		var encoded string
		var created int64
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.TurnID, &m.RunID, &m.Seq, &m.SenderKind, &m.SenderBotID, &encoded, &m.Status, &m.TokenIn, &m.TokenOut, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(encoded), &m.Blocks); err != nil {
			return nil, err
		}
		m.CreatedAt = time.UnixMilli(created).UTC()
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
	return messages, nil
}
func (s *Conversations) MarkRead(ctx context.Context, id string, seq int64) error {
	if seq < 0 {
		return badInput("Read cursor cannot be negative.")
	}
	err := store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		var maximum int64
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0) FROM messages WHERE conversation_id=?", id).Scan(&maximum); err != nil {
			return err
		}
		if seq > maximum {
			return badInput("Read cursor cannot exceed the latest message.")
		}
		if err := store.RequireChanged(tx.ExecContext(ctx, "UPDATE conversations SET last_read_seq=MAX(last_read_seq,?) WHERE id=?", seq, id)); err != nil {
			return err
		}
		c, err := scanConversation(tx.QueryRowContext(ctx, "SELECT "+conversationColumns+" FROM conversations c WHERE c.id=?", id))
		if err != nil {
			return err
		}
		_, err = bus.Append(ctx, tx, "conversation.updated", id, conversationEvent(c))
		return err
	})
	if err == nil {
		s.events.Notify()
	}
	return err
}

type ConversationPatch struct {
	Title      Field[string] `json:"title"`
	IsArchived Field[bool]   `json:"is_archived"`
}

func (s *Conversations) Update(ctx context.Context, id string, patch ConversationPatch, actor string) (c domain.Conversation, result error) {
	result = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		c, err = scanConversation(tx.QueryRowContext(ctx, "SELECT "+conversationColumns+" FROM conversations c WHERE c.id=?", id))
		if err != nil {
			return err
		}
		if patch.Title.Set {
			c.Title = strings.TrimSpace(patch.Title.Value)
		}
		if c.Title == "" || len(c.Title) > 200 {
			return badInput("Conversation title must contain 1 to 200 bytes.")
		}
		if patch.IsArchived.Set {
			c.IsArchived = patch.IsArchived.Value
		}
		if c.BotStatus != "idle" && c.IsArchived {
			return domain.NewError(409, "TURN_IN_PROGRESS", "Cancel the active turn before archiving.")
		}
		c.UpdatedAt = time.Now().UTC()
		if _, err = tx.ExecContext(ctx, "UPDATE conversations SET title=?,is_archived=?,updated_at=? WHERE id=?", c.Title, c.IsArchived, c.UpdatedAt.UnixMilli(), id); err != nil {
			return err
		}
		if err = store.Audit(ctx, tx, actor, "conversation.update", id, map[string]string{}); err != nil {
			return err
		}
		_, err = bus.Append(ctx, tx, "conversation.updated", id, conversationEvent(c))
		return err
	})
	if result == nil {
		s.events.Notify()
	}
	return c, result
}

func conversationEvent(c domain.Conversation) map[string]any {
	return map[string]any{"conversation_id": c.ID, "title": c.Title, "kind": c.Kind, "last_message_preview": c.LastMessagePreview, "last_message_at": c.UpdatedAt, "unread_count": c.UnreadCount, "unread_artifacts": c.UnreadArtifacts, "has_active_turn": c.HasActiveTurn, "bot_status": c.BotStatus, "is_archived": c.IsArchived, "members": c.Members}
}

// AppendConversationEvent 使用同一事务中的消息与执行状态生成侧栏摘要，保持读取与推送口径一致。
func AppendConversationEvent(ctx context.Context, tx *sql.Tx, id string) error {
	c, err := scanConversation(tx.QueryRowContext(ctx, "SELECT "+conversationColumns+" FROM conversations c WHERE c.id=?", id))
	if err != nil {
		return err
	}
	_, err = bus.Append(ctx, tx, "conversation.updated", id, conversationEvent(c))
	return err
}
