package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/store"
)

const DefaultBotID = "tom"

// EnsureDefault 由已完成认证初始化的启动或登录流程调用，消费迁移提供的一次性种子。
// 目录准备、身份和单聊均成功后才删除种子；失败可重试，已删除的助手不会重新出现。
func (s *Bots) EnsureDefault(ctx context.Context) (result error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if result != nil {
			result = fmt.Errorf("初始化默认 Bot %s 失败: %w", DefaultBotID, result)
		}
	}()
	var pending bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM system_state WHERE key='default_bot.pending')").Scan(&pending); err != nil {
		return err
	}
	if !pending {
		return nil
	}
	created := false
	result = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		var raw string
		if err := tx.QueryRowContext(ctx, "SELECT value_json FROM system_state WHERE key='default_bot.pending'").Scan(&raw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		var bot domain.Bot
		if len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &bot) != nil || bot.ID != DefaultBotID || bot.ConversationID != "tom-direct" || bot.WorkspaceDir != "bots/tom" || bot.Name != "Tom" || bot.AvatarPath != nil || bot.ModelID != nil || bot.IsArchived {
			return fmt.Errorf("默认助手初始化种子无效")
		}
		if err := validateBot(&bot); err != nil {
			return err
		}
		// 用户可能已有同名助手；候选数量比活动 Bot 多一，保证保留现有名称并找到空位。
		if err := tx.QueryRowContext(ctx, `WITH candidates(ordinal,name) AS (
SELECT 0,? UNION ALL SELECT ROW_NUMBER() OVER(ORDER BY id),?||' '||ROW_NUMBER() OVER(ORDER BY id) FROM bots WHERE is_archived=0)
SELECT name FROM candidates WHERE NOT EXISTS(SELECT 1 FROM bots WHERE bots.name=candidates.name AND bots.is_archived=0) ORDER BY ordinal LIMIT 1`, bot.Name, bot.Name).Scan(&bot.Name); err != nil {
			return err
		}
		var modelID string
		if err := tx.QueryRowContext(ctx, "SELECT id FROM models WHERE is_default=1").Scan(&modelID); err == nil {
			bot.ModelID = &modelID
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := prepareDefaultDirectory(ctx, s.workspace, bot.WorkspaceDir); err != nil {
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
		now := time.Now().UnixMilli()
		if _, err = tx.ExecContext(ctx, `INSERT INTO bots(id,name,role_title,soul,model_id,tool_policy_json,messaging_policy_json,workspace_dir,accent_hue,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, bot.ID, bot.Name, bot.RoleTitle, bot.Soul, bot.ModelID, string(tools), string(messaging), bot.WorkspaceDir, bot.AccentHue, now, now); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO conversations(id,kind,bot_id,title,created_at,updated_at) VALUES(?,'direct',?,?,?,?)`, bot.ConversationID, bot.ID, bot.Name, now, now); err != nil {
			return err
		}
		if err = store.Audit(ctx, tx, "system", "bot.create", bot.ID, map[string]string{"conversation_id": bot.ConversationID}); err != nil {
			return err
		}
		if _, err = bus.Append(ctx, tx, "conversation.updated", bot.ConversationID, map[string]any{"conversation_id": bot.ConversationID, "bot_id": bot.ID, "title": bot.Name, "unread_count": 0, "bot_status": "idle"}); err != nil {
			return err
		}
		if err = store.RequireChanged(tx.ExecContext(ctx, "DELETE FROM system_state WHERE key='default_bot.pending'")); err != nil {
			return err
		}
		created = true
		return nil
	})
	if result == nil && created {
		s.events.Notify()
	}
	return result
}

// prepareDefaultDirectory 逐级拒绝符号链接及普通文件占位；重试复用已准备的私有目录，不清除用户资料。
func prepareDefaultDirectory(ctx context.Context, workspace, directory string) (result error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	for _, part := range []string{"bots", directory, filepath.Join(directory, "inbox"), filepath.Join(directory, "out"), filepath.Join(directory, "notes"), filepath.Join(directory, ".tom"), filepath.Join(directory, ".tom", "memory")} {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = root.Mkdir(part, 0700); err != nil {
			if !os.IsExist(err) {
				return err
			}
			entry, statErr := root.Lstat(part)
			if statErr != nil {
				return statErr
			}
			if !entry.IsDir() {
				return fmt.Errorf("目录 %s 已被文件或符号链接占用", part)
			}
		}
	}
	return nil
}

// bindDefaultBotModel 仅在默认模型设置成功的同一事务内填充未绑定的内置助手，保留用户已选模型。
func bindDefaultBotModel(ctx context.Context, tx *sql.Tx, modelID, actor string) error {
	result, err := tx.ExecContext(ctx, "UPDATE bots SET model_id=?,updated_at=? WHERE id=? AND model_id IS NULL", modelID, time.Now().UnixMilli(), DefaultBotID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return err
	}
	if err = store.Audit(ctx, tx, actor, "bot.update", DefaultBotID, map[string]string{"model_id": modelID}); err != nil {
		return err
	}
	_, err = bus.Append(ctx, tx, "bot.updated", "", map[string]string{"bot_id": DefaultBotID})
	return err
}
