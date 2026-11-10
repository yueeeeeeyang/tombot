package service

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const memoryCheckInterval = 10 * time.Minute

// Maintain 启动时重建派生索引，随后每十分钟校验；单个 Bot 的坏文件不会阻断其他 Bot 的校正。
func (s *Memories) Maintain(ctx context.Context, logger *slog.Logger) {
	ticker := time.NewTicker(memoryCheckInterval)
	defer ticker.Stop()
	s.maintain(ctx, logger, ticker.C)
}

func (s *Memories) maintain(ctx context.Context, logger *slog.Logger, ticks <-chan time.Time) {
	rebuild := true
	for {
		if err := s.scanAll(ctx, rebuild, logger); err != nil && ctx.Err() == nil {
			logger.Error("私有记忆批量校验失败", "operation", "memory.scan")
		}
		rebuild = false
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

func (s *Memories) scanAll(ctx context.Context, rebuild bool, logger *slog.Logger) error {
	cursor := ""
	for {
		rows, err := s.db.QueryContext(ctx, "SELECT id FROM bots WHERE id>? ORDER BY id LIMIT 100", cursor)
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, id := range ids {
			timeout, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, err = s.Sync(timeout, id, rebuild)
			cancel()
			if err != nil && ctx.Err() == nil {
				logger.Warn("私有记忆校验失败，暂停使用该 Bot 的旧索引", "bot_id", id)
				noticeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
				_, noticeErr := s.events.Publish(noticeCtx, "system.notice", "", map[string]string{"bot_id": id, "level": "warning", "text": "私有记忆校验失败，请在对应 Bot 的记忆页查看或修正记忆文件。"})
				stop()
				if noticeErr != nil {
					logger.Error("记忆校验失败提示保存失败", "bot_id", id)
				}
			}
		}
		if len(ids) < 100 {
			return ctx.Err()
		}
		cursor = ids[len(ids)-1]
	}
}
