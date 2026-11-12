package bots

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"io.yueyang/tom/internal/computerdesktop"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
)

const botColumns = `b.id,b.desktop_id,b.name,b.description,b.avatar_ref,b.category,b.system_prompt,b.tool_permissions,b.approval_mode,b.messaging_policy,b.accent_hue,
	c.id,EXISTS(SELECT 1 FROM tombot.bot_avatars a WHERE a.user_id=b.user_id AND a.bot_id=b.id),b.is_archived,b.revision,b.created_at,b.updated_at`
const botJoin = ` FROM tombot.bots b JOIN tombot.conversations c ON c.user_id=b.user_id AND c.bot_id=b.id AND c.kind='direct' `

func scanBot(row scanner) (Bot, error) {
	var bot Bot
	var permissions, messaging []byte
	err := row.Scan(&bot.ID, &bot.DesktopID, &bot.Name, &bot.Description, &bot.AvatarRef, &bot.Category, &bot.SystemPrompt, &permissions, &bot.ApprovalMode, &messaging, &bot.AccentHue,
		&bot.ConversationID, &bot.HasCustomAvatar, &bot.IsArchived, &bot.Revision, &bot.CreatedAt, &bot.UpdatedAt)
	if err != nil {
		return Bot{}, err
	}
	if err = json.Unmarshal(permissions, &bot.ToolPermissions); err != nil {
		return Bot{}, err
	}
	if err = json.Unmarshal(messaging, &bot.MessagingPolicy); err != nil {
		return Bot{}, err
	}
	return bot, nil
}
func readBot(ctx context.Context, transaction *sql.Tx, userID, id string) (Bot, error) {
	return scanBot(transaction.QueryRowContext(ctx, `SELECT `+botColumns+botJoin+` WHERE b.user_id=$1 AND b.id=$2`, userID, id))
}
func listBots(ctx context.Context, transaction *sql.Tx, userID, before string, limit int, archived bool) (result []Bot, err error) {
	rows, err := transaction.QueryContext(ctx, `SELECT `+botColumns+botJoin+` WHERE b.user_id=$1 AND b.is_archived=$2 AND ($3='' OR b.id<$3) ORDER BY b.id DESC LIMIT $4`, userID, archived, before, limit)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	result = make([]Bot, 0, limit)
	for rows.Next() {
		bot, readErr := scanBot(rows)
		if readErr != nil {
			return nil, readErr
		}
		result = append(result, bot)
	}
	return result, rows.Err()
}

// Get 返回当前用户的单个机器人；其他用户资源与不存在资源采用相同结果。
func (service *Service) Get(ctx context.Context, principal identity.Principal, id string) (Bot, error) {
	if !validID(id) {
		return Bot{}, ErrInvalidInput
	}
	var bot Bot
	err := service.transaction(ctx, principal, false, func(transaction *sql.Tx) error {
		var err error
		bot, err = readBot(ctx, transaction, principal.UserID, id)
		return err
	})
	if err != nil {
		return Bot{}, err
	}
	return bot, nil
}

// List 使用稳定身份分页，不读取头像正文或按每个机器人补查状态。
func (service *Service) List(ctx context.Context, principal identity.Principal, before string, limit int, archived bool) ([]Bot, error) {
	if !validPage(before, limit) {
		return nil, ErrInvalidInput
	}
	var result []Bot
	err := service.transaction(ctx, principal, false, func(transaction *sql.Tx) error {
		var err error
		result, err = listBots(ctx, transaction, principal.UserID, before, limit, archived)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Create 原子创建机器人、桌面身份、唯一单聊、工作空间选择与事件；同键重试只返回同一资源。
// 布尔返回值只在本次新建时为真；相同键不同规范化配置返回冲突。
func (service *Service) Create(ctx context.Context, principal identity.Principal, request CreateRequest) (Bot, bool, error) {
	if !validID(request.ClientRequestID) {
		return Bot{}, false, ErrInvalidInput
	}
	spec, err := service.normalize(request.Spec)
	if err != nil {
		return Bot{}, false, err
	}
	digest, err := requestDigest(spec)
	if err != nil {
		return Bot{}, false, err
	}
	var bot Bot
	created := false
	err = service.transaction(ctx, principal, true, func(transaction *sql.Tx) error {
		var existing string
		var previous []byte
		err := transaction.QueryRowContext(ctx, `SELECT id,creation_digest FROM tombot.bots WHERE user_id=$1 AND client_request_id=$2`, principal.UserID, request.ClientRequestID).Scan(&existing, &previous)
		if err == nil {
			if !bytes.Equal(previous, digest) {
				return ErrConflict
			}
			bot, err = readBot(ctx, transaction, principal.UserID, existing)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var count int
		if err = transaction.QueryRowContext(ctx, `SELECT count(*) FROM tombot.bots WHERE user_id=$1`, principal.UserID).Scan(&count); err != nil {
			return err
		}
		if count >= service.options.MaxBots {
			return ErrCapacity
		}
		permissions, err := json.Marshal(spec.ToolPermissions)
		if err != nil {
			return err
		}
		messaging, err := json.Marshal(spec.MessagingPolicy)
		if err != nil {
			return err
		}
		botID, desktopID := ids.New(), ids.New()
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.bots(user_id,id,name,description,avatar_ref,category,system_prompt,tool_permissions,approval_mode,messaging_policy,accent_hue,client_request_id,creation_digest,desktop_id)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, principal.UserID, botID, spec.Name, spec.Description, spec.AvatarRef, spec.Category, spec.SystemPrompt, permissions, spec.ApprovalMode, messaging, spec.AccentHue, request.ClientRequestID, digest, desktopID); err != nil {
			return err
		}
		if _, err = computerdesktop.Register(ctx, transaction, principal.UserID, botID, desktopID); err != nil {
			if errors.Is(err, computerdesktop.ErrCapacity) {
				return ErrCapacity
			}
			return err
		}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.conversations(user_id,id,kind,bot_id,title) VALUES($1,$2,'direct',$3,$4)`, principal.UserID, ids.New(), botID, spec.Name); err != nil {
			return err
		}
		bot, err = readBot(ctx, transaction, principal.UserID, botID)
		if err != nil {
			return err
		}
		if err = emitChange(ctx, transaction, principal.UserID, "bot.created", bot.ID, bot.Revision); err != nil {
			return err
		}
		var revision int64
		err = transaction.QueryRowContext(ctx, `UPDATE tombot.user_workspaces SET active_bot_id=COALESCE(active_bot_id,$2),primary_bot_id=COALESCE(primary_bot_id,$2),revision=revision+1,updated_at=clock_timestamp()
			WHERE user_id=$1 AND (active_bot_id IS NULL OR primary_bot_id IS NULL) RETURNING revision`, principal.UserID, bot.ID).Scan(&revision)
		if err == nil {
			err = emitChange(ctx, transaction, principal.UserID, "workspace.updated", principal.UserID, revision)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return Bot{}, false, err
	}
	return bot, created, nil
}

// Update 替换单个机器人的配置，修订冲突或已归档时不修改任何关联资源。
func (service *Service) Update(ctx context.Context, principal identity.Principal, id string, request UpdateRequest) (Bot, error) {
	if !validID(id) || request.ExpectedRevision < 1 {
		return Bot{}, ErrInvalidInput
	}
	spec, err := service.normalize(request.Spec)
	if err != nil {
		return Bot{}, err
	}
	var bot Bot
	err = service.transaction(ctx, principal, true, func(transaction *sql.Tx) error {
		current, err := readBot(ctx, transaction, principal.UserID, id)
		if err != nil {
			return err
		}
		if current.Revision != request.ExpectedRevision {
			return ErrConflict
		}
		if current.IsArchived {
			return ErrArchived
		}
		permissions, err := json.Marshal(spec.ToolPermissions)
		if err != nil {
			return err
		}
		messaging, err := json.Marshal(spec.MessagingPolicy)
		if err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.bots SET name=$3,description=$4,avatar_ref=$5,category=$6,system_prompt=$7,tool_permissions=$8,approval_mode=$9,messaging_policy=$10,accent_hue=$11,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, principal.UserID, id, spec.Name, spec.Description, spec.AvatarRef, spec.Category, spec.SystemPrompt, permissions, spec.ApprovalMode, messaging, spec.AccentHue); err != nil {
			return err
		}
		if current.Name != spec.Name {
			if _, err = transaction.ExecContext(ctx, `UPDATE tombot.conversations SET title=$3,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND bot_id=$2 AND kind='direct'`, principal.UserID, id, spec.Name); err != nil {
				return err
			}
		}
		bot, err = readBot(ctx, transaction, principal.UserID, id)
		if err != nil {
			return err
		}
		return emitChange(ctx, transaction, principal.UserID, "bot.updated", id, bot.Revision)
	})
	if err != nil {
		return Bot{}, err
	}
	return bot, nil
}

// SetArchived 保留历史并清除工作空间中失效的选择；恢复仍受用户内名称唯一约束保护。
func (service *Service) SetArchived(ctx context.Context, principal identity.Principal, id string, revision int64, archived bool) (Bot, error) {
	if !validID(id) || revision < 1 {
		return Bot{}, ErrInvalidInput
	}
	var bot Bot
	err := service.transaction(ctx, principal, true, func(transaction *sql.Tx) error {
		var err error
		bot, err = readBot(ctx, transaction, principal.UserID, id)
		if err != nil {
			return err
		}
		if bot.Revision != revision {
			return ErrConflict
		}
		if bot.IsArchived == archived {
			return nil
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.bots SET is_archived=$3,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, principal.UserID, id, archived); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.conversations SET is_archived=$3,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND bot_id=$2 AND kind='direct'`, principal.UserID, id, archived); err != nil {
			return err
		}
		if archived {
			var workspaceRevision int64
			err = transaction.QueryRowContext(ctx, `UPDATE tombot.user_workspaces SET active_bot_id=NULLIF(active_bot_id,$2),primary_bot_id=NULLIF(primary_bot_id,$2),revision=revision+1,updated_at=clock_timestamp()
				WHERE user_id=$1 AND (active_bot_id=$2 OR primary_bot_id=$2) RETURNING revision`, principal.UserID, id).Scan(&workspaceRevision)
			if err == nil {
				err = emitChange(ctx, transaction, principal.UserID, "workspace.updated", principal.UserID, workspaceRevision)
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		bot, err = readBot(ctx, transaction, principal.UserID, id)
		if err != nil {
			return err
		}
		return emitChange(ctx, transaction, principal.UserID, "bot.updated", id, bot.Revision)
	})
	if err != nil {
		return Bot{}, err
	}
	return bot, nil
}
