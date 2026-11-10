package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/store"
)

const desktopSoul = "You are the user's cat assistant running on Tom. Think, use tools when they help, and stay inside your workspace. Reply in the user's language. When the user asks you to remember or forget, call memory_write or memory_delete. When the user asks for a reminder or a recurring task, call routine_upsert or routine_delete. When you need another cat, call message_bot. Do not ask the user to edit memory, schedule, or private-message pages."

func desktopToolPolicy() domain.ToolPolicy {
	return domain.ToolPolicy{AllowedTools: []string{"file_read", "file_list", "file_write", "computer_shell", "browser", "terminal_session", "memory_search", "memory_write", "memory_delete", "routine_list", "routine_upsert", "routine_delete", "message_bot"}, Approval: "loose"}
}

const desktopMessaging = `{"accept_dm":true,"can_initiate":true,"accept_group_invite":true}`

func (s *Service) ensureState(ctx context.Context) (stateRow, error) {
	row, err := s.readState(ctx)
	if err == nil {
		return row, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return stateRow{}, err
	}
	workspace := Workspace{Version: 5, ActiveAgentID: "", Agents: []Agent{}}
	settings := DefaultModelForm()
	if err = s.writeState(ctx, stateRow{workspace: workspace, revision: 1, pristine: true, settings: settings}); err != nil {
		if row, readErr := s.readState(ctx); readErr == nil {
			return row, nil
		}
		return stateRow{}, err
	}
	return stateRow{workspace: workspace, revision: 1, pristine: true, settings: settings}, nil
}

func (s *Service) readState(ctx context.Context) (stateRow, error) {
	var active string
	var revision int64
	var pristine int
	var settings ModelForm
	var streaming int
	var nonce, ciphertext []byte
	err := s.db.QueryRowContext(ctx, `SELECT active_agent_id, revision, pristine, language, provider, provider_name, base_url, model_name, temperature, top_p, max_output_tokens, is_streaming, api_key_nonce, api_key_ciphertext FROM workspace_meta WHERE id=1`).Scan(
		&active, &revision, &pristine, &settings.Language, &settings.Provider, &settings.ProviderName, &settings.BaseURL, &settings.ModelName, &settings.Temperature, &settings.TopP, &settings.MaxOutputTokens, &streaming, &nonce, &ciphertext,
	)
	if err != nil {
		return stateRow{}, err
	}
	settings.IsStreaming = streaming == 1
	agents, err := s.readAgents(ctx)
	if err != nil {
		return stateRow{}, err
	}
	if agents == nil {
		agents = []Agent{}
	}
	if len(agents) == 0 {
		active = ""
	}
	return stateRow{
		workspace: Workspace{Version: 5, ActiveAgentID: active, Agents: agents},
		revision:  revision, pristine: pristine == 1, settings: settings, nonce: nonce, ciphertext: ciphertext,
	}, nil
}

func (s *Service) readAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, desktop_id, name, description, group_name, avatar_preset, catalog_model_id, draft, computer_url FROM bots WHERE desktop_id IS NOT NULL ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agents := []Agent{}
	for rows.Next() {
		var agent Agent
		var desktopID string
		if err = rows.Scan(&agent.ID, &desktopID, &agent.Name, &agent.Description, &agent.Group, &agent.Avatar, &agent.ModelID, &agent.Draft, &agent.ComputerURL); err != nil {
			return nil, err
		}
		if desktopID != "" {
			agent.ID = desktopID
		}
		agent.Messages = []ChatMessage{}
		agent.Artifacts = []Artifact{}
		agents = append(agents, agent)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	for index := range agents {
		messages, err := s.readMessages(ctx, agents[index].ID)
		if err != nil {
			return nil, err
		}
		artifacts, err := s.readArtifacts(ctx, agents[index].ID)
		if err != nil {
			return nil, err
		}
		agents[index].Messages = messages
		agents[index].Artifacts = artifacts
	}
	return agents, nil
}

func (s *Service) readMessages(ctx context.Context, agentID string) ([]ChatMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT messages.id, messages.sender_kind, messages.blocks_json, messages.status, messages.created_at, messages.request_id, COALESCE(messages.reply_to_id,''), bots.id FROM messages JOIN conversations ON conversations.id=messages.conversation_id JOIN bots ON bots.id=conversations.bot_id WHERE conversations.kind='direct' AND (bots.desktop_id=? OR bots.id=?) ORDER BY messages.created_at, messages.seq`, agentID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := []ChatMessage{}
	for rows.Next() {
		var id, sender, encoded, status, requestID, replyID, botID string
		var created int64
		if err = rows.Scan(&id, &sender, &encoded, &status, &created, &requestID, &replyID, &botID); err != nil {
			return nil, err
		}
		content := textFromBlocks(encoded)
		attachments := attachmentsFromBlocks(encoded)
		notice := dmNoticeFromBlocks(encoded, botID)
		if strings.TrimSpace(content) == "" && len(attachments) == 0 && notice == nil {
			continue
		}
		message := ChatMessage{
			ID: id, Role: "assistant", Content: content, CreatedAt: time.UnixMilli(created).UTC().Format("2006-01-02T15:04:05.000Z07:00"),
			Status: desktopStatus(status), Reactions: []Reaction{}, Attachments: attachments, DmNotice: notice,
		}
		if sender == "user" {
			message.Role = "user"
		}
		if requestID != "" {
			message.RequestID = requestID
		}
		if replyID != "" {
			message.ReplyTo = &Reply{MessageID: replyID, Excerpt: ""}
		}
		messages = append(messages, message)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	for index := range messages {
		if messages[index].ReplyTo != nil {
			excerpt := s.replyExcerpt(ctx, messages[index].ReplyTo.MessageID)
			if excerpt == "" {
				messages[index].ReplyTo = nil
			} else {
				messages[index].ReplyTo.Excerpt = excerpt
			}
		}
		reactions, err := s.readReactions(ctx, messages[index].ID)
		if err != nil {
			return nil, err
		}
		messages[index].Reactions = reactions
		if messages[index].DmNotice == nil || messages[index].DmNotice.PeerName != "" {
			continue
		}
		var peerName string
		if err = s.db.QueryRowContext(ctx, `SELECT name FROM bots WHERE id=?`, messages[index].DmNotice.PeerID).Scan(&peerName); err != nil {
			peerName = messages[index].DmNotice.PeerID
		}
		messages[index].DmNotice.PeerName = peerName
		if messages[index].DmNotice.Direction == "received" {
			messages[index].Content = "收到 " + peerName + " 的消息"
		} else {
			messages[index].Content = "已发消息给 " + peerName
		}
	}
	return messages, nil
}

func dmNoticeFromBlocks(encoded, botID string) *DmNotice {
	var blocks []domain.MessageBlock
	if json.Unmarshal([]byte(encoded), &blocks) != nil {
		return nil
	}
	for _, block := range blocks {
		if block.Kind != "handoff" || block.Handoff == nil || block.Handoff.ConversationID == "" || block.Handoff.ErrorCode != "" {
			continue
		}
		notice := &DmNotice{Direction: "sent", PeerID: block.Handoff.ToBotID, ConversationID: block.Handoff.ConversationID}
		if block.Handoff.ToBotID == botID {
			notice.Direction = "received"
			notice.PeerID = block.Handoff.FromBotID
		} else if block.Handoff.FromBotID != botID {
			return nil
		}
		return notice
	}
	return nil
}

func (s *Service) readReactions(ctx context.Context, messageID string) ([]Reaction, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT emoji, count FROM message_reactions WHERE message_id=? ORDER BY emoji`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reactions := []Reaction{}
	for rows.Next() {
		var reaction Reaction
		if err = rows.Scan(&reaction.Emoji, &reaction.Count); err != nil {
			return nil, err
		}
		reactions = append(reactions, reaction)
	}
	return reactions, rows.Err()
}

func (s *Service) readArtifacts(ctx context.Context, agentID string) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT bot_artifacts.id, title, kind, preview, bot_artifacts.created_at FROM bot_artifacts JOIN bots ON bots.id=bot_artifacts.bot_id WHERE bots.desktop_id=? OR bots.id=? ORDER BY bot_artifacts.created_at, bot_artifacts.id`, agentID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	artifacts := []Artifact{}
	for rows.Next() {
		var artifact Artifact
		var created int64
		if err = rows.Scan(&artifact.ID, &artifact.Title, &artifact.Kind, &artifact.Preview, &created); err != nil {
			return nil, err
		}
		artifact.CreatedAt = time.UnixMilli(created).UTC().Format("2006-01-02T15:04:05.000Z07:00")
		artifacts = append(artifacts, artifact)
	}
	return artifacts, rows.Err()
}

func (s *Service) replyExcerpt(ctx context.Context, messageID string) string {
	var encoded string
	if err := s.db.QueryRowContext(ctx, `SELECT blocks_json FROM messages WHERE id=?`, messageID).Scan(&encoded); err != nil {
		return ""
	}
	text := strings.TrimSpace(textFromBlocks(encoded))
	runes := []rune(text)
	if len(runes) > 80 {
		text = string(runes[:80])
	}
	return text
}

func (s *Service) writeState(ctx context.Context, row stateRow) error {
	if err := s.ensureDirectories(row.workspace); err != nil {
		return err
	}
	return store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		pristine := 0
		if row.pristine {
			pristine = 1
		}
		streaming := 0
		if row.settings.IsStreaming {
			streaming = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_meta(id,active_agent_id,revision,pristine,language,provider,provider_name,base_url,model_name,temperature,top_p,max_output_tokens,is_streaming,api_key_nonce,api_key_ciphertext,updated_at) VALUES(1,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET active_agent_id=excluded.active_agent_id, revision=excluded.revision, pristine=excluded.pristine, language=excluded.language, provider=excluded.provider, provider_name=excluded.provider_name, base_url=excluded.base_url, model_name=excluded.model_name, temperature=excluded.temperature, top_p=excluded.top_p, max_output_tokens=excluded.max_output_tokens, is_streaming=excluded.is_streaming, api_key_nonce=excluded.api_key_nonce, api_key_ciphertext=excluded.api_key_ciphertext, updated_at=excluded.updated_at`,
			row.workspace.ActiveAgentID, row.revision, pristine, row.settings.Language, row.settings.Provider, row.settings.ProviderName, row.settings.BaseURL, row.settings.ModelName, row.settings.Temperature, row.settings.TopP, row.settings.MaxOutputTokens, streaming, nullableBytes(row.nonce), nullableBytes(row.ciphertext), s.now().UnixMilli()); err != nil {
			return err
		}
		kept := map[string]struct{}{}
		for index, agent := range row.workspace.Agents {
			botID, err := s.upsertAgent(ctx, tx, agent, index)
			if err != nil {
				return err
			}
			kept[botID] = struct{}{}
			if err = s.syncAgentMessages(ctx, tx, botID, agent.Messages); err != nil {
				return err
			}
			if err = s.syncArtifacts(ctx, tx, botID, agent.Artifacts); err != nil {
				return err
			}
		}
		return s.archiveMissingAgents(ctx, tx, kept)
	})
}

func (s *Service) upsertAgent(ctx context.Context, tx *sql.Tx, agent Agent, index int) (string, error) {
	var botID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM bots WHERE id=? OR desktop_id=?`, agent.ID, agent.ID).Scan(&botID)
	now := s.now().UnixMilli()
	tools, marshalErr := json.Marshal(desktopToolPolicy())
	if marshalErr != nil {
		return "", marshalErr
	}
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO bots(id,name,role_title,soul,tool_policy_json,messaging_policy_json,workspace_dir,accent_hue,created_at,updated_at,desktop_id,group_name,description,avatar_preset,catalog_model_id,draft,computer_url) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			agent.ID, agent.Name, fitRole(agent.Description), desktopSoul, string(tools), desktopMessaging, "bots/"+agent.ID, (index*40)%360, now+int64(index), now+int64(index), agent.ID, agent.Group, agent.Description, agent.Avatar, agent.ModelID, agent.Draft, agent.ComputerURL); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO conversations(id,kind,source,bot_id,title,created_at,updated_at) VALUES(?,'direct','user',?,?,?,?)`, "conv-"+agent.ID, agent.ID, agent.Name, now, now); err != nil {
			return "", err
		}
		return agent.ID, nil
	}
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE bots SET name=?, role_title=?, soul=?, tool_policy_json=?, messaging_policy_json=?, desktop_id=?, group_name=?, description=?, avatar_preset=?, catalog_model_id=?, draft=?, computer_url=?, updated_at=? WHERE id=?`,
		agent.Name, fitRole(agent.Description), desktopSoul, string(tools), desktopMessaging, agent.ID, agent.Group, agent.Description, agent.Avatar, agent.ModelID, agent.Draft, agent.ComputerURL, now, botID); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE conversations SET title=?, updated_at=? WHERE bot_id=? AND kind='direct'`, agent.Name, now, botID); err != nil {
		return "", err
	}
	return botID, nil
}

func (s *Service) syncAgentMessages(ctx context.Context, tx *sql.Tx, botID string, messages []ChatMessage) error {
	var conversationID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM conversations WHERE bot_id=? AND kind='direct' AND source!='routine'`, botID).Scan(&conversationID); err != nil {
		return err
	}
	kept := make([]any, 0, len(messages))
	for _, message := range messages {
		kept = append(kept, message.ID)
		if err := s.upsertMessage(ctx, tx, conversationID, message); err != nil {
			return err
		}
	}
	// 只删除桌面自己写过、又从界面拿掉的消息。agent 的 complete 记录留在同一张表里。
	query := `DELETE FROM messages WHERE conversation_id=? AND status IN ('unsent','failed','sent')`
	args := []any{conversationID}
	if len(kept) > 0 {
		query += ` AND id NOT IN (` + placeholders(len(kept)) + `)`
		args = append(args, kept...)
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return err
	}
	return nil
}

func (s *Service) upsertMessage(ctx context.Context, tx *sql.Tx, conversationID string, message ChatMessage) error {
	blocks := []domain.MessageBlock{{Kind: "text", Text: message.Content}}
	for _, item := range message.Attachments {
		blocks = append(blocks, domain.MessageBlock{Kind: "artifact", Artifact: &domain.ArtifactRef{AttachmentInput: domain.AttachmentInput{Path: item.Path, SHA256: item.SHA256}, MediaType: item.MediaType}})
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return err
	}
	created, err := time.Parse(time.RFC3339Nano, message.CreatedAt)
	if err != nil {
		created, err = time.Parse(time.RFC3339, message.CreatedAt)
		if err != nil {
			return domain.NewError(400, "MESSAGE_INVALID", "消息时间无效")
		}
	}
	sender := "user"
	if message.Role == "assistant" {
		sender = "bot"
	}
	var reply any
	if message.ReplyTo != nil && message.ReplyTo.MessageID != "" && message.ReplyTo.MessageID != message.ID {
		var exists int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE id=?`, message.ReplyTo.MessageID).Scan(&exists); err != nil {
			return err
		}
		if exists == 1 {
			reply = message.ReplyTo.MessageID
		}
	}
	var existing, existingStatus string
	err = tx.QueryRowContext(ctx, `SELECT blocks_json, status FROM messages WHERE id=?`, message.ID).Scan(&existing, &existingStatus)
	if errors.Is(err, sql.ErrNoRows) {
		var seq int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM messages WHERE conversation_id=?`, conversationID).Scan(&seq); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO messages(id,conversation_id,seq,sender_kind,blocks_json,status,created_at,request_id,reply_to_id) VALUES(?,?,?,?,?,?,?,?,?)`,
			message.ID, conversationID, seq, sender, string(encoded), message.Status, created.UnixMilli(), message.RequestID, reply); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if existingStatus == "complete" || existingStatus == "streaming" || strings.Contains(existing, `"tool_use"`) || strings.Contains(existing, `"tool_result"`) {
		if _, err = tx.ExecContext(ctx, `UPDATE messages SET request_id=? WHERE id=?`, message.RequestID, message.ID); err != nil {
			return err
		}
	} else if _, err = tx.ExecContext(ctx, `UPDATE messages SET blocks_json=?, status=?, request_id=?, reply_to_id=?, created_at=? WHERE id=?`, string(encoded), message.Status, message.RequestID, reply, created.UnixMilli(), message.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM message_reactions WHERE message_id=?`, message.ID); err != nil {
		return err
	}
	for _, reaction := range message.Reactions {
		if _, err = tx.ExecContext(ctx, `INSERT INTO message_reactions(message_id,emoji,count) VALUES(?,?,?)`, message.ID, reaction.Emoji, reaction.Count); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) syncArtifacts(ctx context.Context, tx *sql.Tx, botID string, artifacts []Artifact) error {
	kept := make([]any, 0, len(artifacts))
	for _, artifact := range artifacts {
		kept = append(kept, artifact.ID)
		created, err := time.Parse(time.RFC3339Nano, artifact.CreatedAt)
		if err != nil {
			created, err = time.Parse(time.RFC3339, artifact.CreatedAt)
			if err != nil {
				return domain.NewError(400, "WORKSPACE_INVALID", "产物时间无效")
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO bot_artifacts(id,bot_id,title,kind,preview,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET bot_id=excluded.bot_id, title=excluded.title, kind=excluded.kind, preview=excluded.preview, created_at=excluded.created_at`,
			artifact.ID, botID, artifact.Title, artifact.Kind, artifact.Preview, created.UnixMilli()); err != nil {
			return err
		}
	}
	query := `DELETE FROM bot_artifacts WHERE bot_id=?`
	args := []any{botID}
	if len(kept) > 0 {
		query += ` AND id NOT IN (` + placeholders(len(kept)) + `)`
		args = append(args, kept...)
	}
	_, err := tx.ExecContext(ctx, query, args...)
	return err
}

func (s *Service) archiveMissingAgents(ctx context.Context, tx *sql.Tx, kept map[string]struct{}) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM bots WHERE desktop_id IS NOT NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return err
		}
		if _, ok := kept[id]; ok {
			continue
		}
		if _, err = tx.ExecContext(ctx, `UPDATE bots SET is_archived=1, desktop_id=NULL WHERE id=?`, id); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Service) ensureDirectories(workspace Workspace) error {
	if s.workspaceDir == "" {
		return nil
	}
	for _, agent := range workspace.Agents {
		directory := filepath.Join(s.workspaceDir, "bots", agent.ID)
		for _, part := range []string{"", "inbox", "out", "notes", filepath.Join(".tom", "memory")} {
			if err := os.MkdirAll(filepath.Join(directory, part), 0o700); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) findUserTwin(ctx context.Context, agentID, content, exceptID string) (string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT messages.id, messages.blocks_json FROM messages JOIN conversations ON conversations.id=messages.conversation_id JOIN bots ON bots.id=conversations.bot_id WHERE (bots.desktop_id=? OR bots.id=?) AND messages.sender_kind='user' AND messages.id!=? AND messages.status='complete' ORDER BY messages.seq DESC`, agentID, agentID, exceptID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var id, encoded string
		if err = rows.Scan(&id, &encoded); err != nil {
			return "", err
		}
		if strings.TrimSpace(textFromBlocks(encoded)) == strings.TrimSpace(content) {
			return id, nil
		}
	}
	return "", rows.Err()
}

func (s *Service) findAssistantMessage(ctx context.Context, agentID, reply string) (string, error) {
	var id, encoded string
	err := s.db.QueryRowContext(ctx, `SELECT messages.id, messages.blocks_json FROM messages JOIN conversations ON conversations.id=messages.conversation_id JOIN bots ON bots.id=conversations.bot_id WHERE (bots.desktop_id=? OR bots.id=?) AND messages.sender_kind='bot' ORDER BY messages.seq DESC LIMIT 1`, agentID, agentID).Scan(&id, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(textFromBlocks(encoded)) != reply {
		return "", nil
	}
	return id, nil
}

func attachmentsFromBlocks(encoded string) []MessageAttachment {
	var blocks []domain.MessageBlock
	if json.Unmarshal([]byte(encoded), &blocks) != nil {
		return nil
	}
	attachments := []MessageAttachment{}
	for _, block := range blocks {
		if block.Kind != "artifact" || block.Artifact == nil || block.Artifact.Path == "" {
			continue
		}
		name := block.Artifact.Path
		if slash := strings.LastIndex(name, "/"); slash >= 0 {
			name = name[slash+1:]
		}
		kind := "file"
		if block.Artifact.IsImage() {
			kind = "image"
		}
		attachments = append(attachments, MessageAttachment{Name: name, Path: block.Artifact.Path, SHA256: block.Artifact.SHA256, MediaType: block.Artifact.MediaType, Kind: kind})
	}
	return attachments
}

func textFromBlocks(encoded string) string {
	var blocks []domain.MessageBlock
	if json.Unmarshal([]byte(encoded), &blocks) != nil {
		return ""
	}
	var builder strings.Builder
	for _, block := range blocks {
		if block.Kind == "text" {
			builder.WriteString(block.Text)
		}
	}
	return builder.String()
}

func desktopStatus(status string) string {
	switch status {
	case "unsent":
		return "unsent"
	case "failed", "interrupted", "canceled":
		return "failed"
	default:
		return "sent"
	}
}

func placeholders(count int) string {
	marks := make([]string, count)
	for index := range marks {
		marks[index] = "?"
	}
	return strings.Join(marks, ",")
}
