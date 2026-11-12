package bots

import (
	"context"
	"database/sql"
	"errors"

	"io.yueyang/tom/internal/identity"
)

func readWorkspace(ctx context.Context, transaction *sql.Tx, userID string) (Workspace, error) {
	var workspace Workspace
	err := transaction.QueryRowContext(ctx, `SELECT active_bot_id,primary_bot_id,revision FROM tombot.user_workspaces WHERE user_id=$1`, userID).Scan(&workspace.ActiveBotID, &workspace.PrimaryBotID, &workspace.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return Workspace{Revision: 1}, nil
	}
	return workspace, err
}

// Snapshot 读取有限目录首页和提交水位；后续目录分页继续使用各资源的稳定身份游标。
func (service *Service) Snapshot(ctx context.Context, principal identity.Principal, limit int) (Snapshot, error) {
	if !validPage("", limit) {
		return Snapshot{}, ErrInvalidInput
	}
	var snapshot Snapshot
	err := service.transaction(ctx, principal, false, func(transaction *sql.Tx) error {
		var err error
		snapshot.Workspace, err = readWorkspace(ctx, transaction, principal.UserID)
		if err != nil {
			return err
		}
		snapshot.Bots, err = listBots(ctx, transaction, principal.UserID, "", limit+1, false)
		if err != nil {
			return err
		}
		snapshot.Groups, err = listGroups(ctx, transaction, principal.UserID, "", limit+1, false)
		if err != nil {
			return err
		}
		snapshot.HasMoreBots = len(snapshot.Bots) > limit
		snapshot.HasMoreGroups = len(snapshot.Groups) > limit
		if snapshot.HasMoreBots {
			snapshot.Bots = snapshot.Bots[:limit]
		}
		if snapshot.HasMoreGroups {
			snapshot.Groups = snapshot.Groups[:limit]
		}
		return transaction.QueryRowContext(ctx, `SELECT COALESCE((SELECT sequence FROM tombot.user_event_heads WHERE user_id=$1),0)`, principal.UserID).Scan(&snapshot.EventSequence)
	})
	if err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

// UpdateWorkspace 只修改选择；空指针明确清空，选中的机器人必须属于本人且仍有效。
func (service *Service) UpdateWorkspace(ctx context.Context, principal identity.Principal, request WorkspaceRequest) (Workspace, error) {
	if request.ExpectedRevision < 1 {
		return Workspace{}, ErrInvalidInput
	}
	for _, id := range []*string{request.ActiveBotID, request.PrimaryBotID} {
		if id != nil && !validID(*id) {
			return Workspace{}, ErrInvalidInput
		}
	}
	var workspace Workspace
	err := service.transaction(ctx, principal, true, func(transaction *sql.Tx) error {
		current, err := readWorkspace(ctx, transaction, principal.UserID)
		if err != nil {
			return err
		}
		if current.Revision != request.ExpectedRevision {
			return ErrConflict
		}
		for _, id := range []*string{request.ActiveBotID, request.PrimaryBotID} {
			if id == nil {
				continue
			}
			var archived bool
			if err = transaction.QueryRowContext(ctx, `SELECT is_archived FROM tombot.bots WHERE user_id=$1 AND id=$2`, principal.UserID, *id).Scan(&archived); err != nil {
				return err
			}
			if archived {
				return ErrArchived
			}
		}
		err = transaction.QueryRowContext(ctx, `UPDATE tombot.user_workspaces SET active_bot_id=$2,primary_bot_id=$3,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 RETURNING active_bot_id,primary_bot_id,revision`, principal.UserID, request.ActiveBotID, request.PrimaryBotID).Scan(&workspace.ActiveBotID, &workspace.PrimaryBotID, &workspace.Revision)
		if err != nil {
			return err
		}
		return emitChange(ctx, transaction, principal.UserID, "workspace.updated", principal.UserID, workspace.Revision)
	})
	if err != nil {
		return Workspace{}, err
	}
	return workspace, nil
}

const conversationColumns = `id,kind,bot_id,group_id,peer_bot_id,title,message_sequence,last_read_sequence,is_archived,revision,created_at,updated_at`

func scanConversation(row scanner) (Conversation, error) {
	var conversation Conversation
	err := row.Scan(&conversation.ID, &conversation.Kind, &conversation.BotID, &conversation.GroupID, &conversation.PeerBotID, &conversation.Title, &conversation.MessageSequence, &conversation.LastReadSequence, &conversation.IsArchived, &conversation.Revision, &conversation.CreatedAt, &conversation.UpdatedAt)
	return conversation, err
}

// GetConversation 仅返回本人会话的目录状态；消息正文由独立消息领域提供。
func (service *Service) GetConversation(ctx context.Context, principal identity.Principal, id string) (Conversation, error) {
	if !validID(id) {
		return Conversation{}, ErrInvalidInput
	}
	var conversation Conversation
	err := service.transaction(ctx, principal, false, func(transaction *sql.Tx) error {
		var err error
		conversation, err = scanConversation(transaction.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM tombot.conversations WHERE user_id=$1 AND id=$2`, principal.UserID, id))
		return err
	})
	if err != nil {
		return Conversation{}, err
	}
	return conversation, nil
}

// ListConversations 有界读取目录，查询从数据库内限定用户与归档范围。
func (service *Service) ListConversations(ctx context.Context, principal identity.Principal, before string, limit int, archived bool) ([]Conversation, error) {
	if !validPage(before, limit) {
		return nil, ErrInvalidInput
	}
	conversations := make([]Conversation, 0, limit)
	err := service.transaction(ctx, principal, false, func(transaction *sql.Tx) (result error) {
		rows, err := transaction.QueryContext(ctx, `SELECT `+conversationColumns+` FROM tombot.conversations WHERE user_id=$1 AND is_archived=$2 AND ($3='' OR id<$3) ORDER BY id DESC LIMIT $4`, principal.UserID, archived, before, limit)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, rows.Close()) }()
		for rows.Next() {
			conversation, err := scanConversation(rows)
			if err != nil {
				return err
			}
			conversations = append(conversations, conversation)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return conversations, nil
}
