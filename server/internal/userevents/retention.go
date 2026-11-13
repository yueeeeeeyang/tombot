package userevents

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"io.yueyang/tom/internal/store/postgres"
)

// ErrCursorExpired 表示客户端游标早于已清理边界；调用方必须重新读取快照，
// 不能把缺失的历史事件当成"这段时间没有变化"。
var ErrCursorExpired = errors.New("事件游标已过期")

// maxRetentionBatches 限制单轮清理的事务数，避免积压时长时间占用控制连接。
const maxRetentionBatches = 20

// RetentionOptions 控制清理节奏；保留条数、批量与间隔都必须有界。
type RetentionOptions struct {
	Retention int
	BatchSize int
	Interval  time.Duration
	Timeout   time.Duration
	WorkerID  string
}

// Validate 供启动配置在连接数据库前拒绝无效的清理参数。
func (options RetentionOptions) Validate() error {
	if options.Retention < 100 || options.Retention > 1_000_000 {
		return errors.New("事件保留条数无效")
	}
	if options.BatchSize < 1 || options.BatchSize > 10_000 {
		return errors.New("事件清理批量无效")
	}
	if options.Interval < time.Second || options.Interval > 24*time.Hour {
		return errors.New("事件清理间隔无效")
	}
	if options.Timeout < time.Second || options.Timeout > time.Minute {
		return errors.New("事件清理超时无效")
	}
	if options.WorkerID == "" {
		return errors.New("事件清理缺少实例标识")
	}
	return nil
}

// Retention 按保留条数清理历史事件。
// 它使用控制连接：清理跨用户进行，不能借用任何单个用户的租户身份。
type Retention struct {
	database *postgres.Database
	options  RetentionOptions
	logger   *slog.Logger
}

// NewRetention 只接受控制连接；租户连接看不到其他用户的事件头。
func NewRetention(database *postgres.Database, options RetentionOptions, logger *slog.Logger) (*Retention, error) {
	if database == nil || database.Role() != postgres.ControlRole {
		return nil, errors.New("事件清理需要控制数据库连接")
	}
	if logger == nil {
		return nil, errors.New("事件清理缺少日志")
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	return &Retention{database: database, options: options, logger: logger}, nil
}

// Run 周期清理直到上下文结束。
// 单次失败只记录并等待下一个周期，避免一次数据库抖动让清理永久停摆；
// 清理幂等，重试同一批用户是安全的。
func (retention *Retention) Run(ctx context.Context) error {
	ticker := time.NewTicker(retention.options.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		if err := retention.RunOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			retention.logger.ErrorContext(ctx, "用户事件清理失败", "worker_id", retention.options.WorkerID, "reason", err.Error())
		}
	}
}

// RunOnce 每事务清理一批待清理用户，直到没有更多用户或达到批次数上限。
func (retention *Retention) RunOnce(ctx context.Context) (result error) {
	ctx, cancel := context.WithTimeout(ctx, retention.options.Timeout)
	defer cancel()
	for range maxRetentionBatches {
		pruned := 0
		err := retention.database.Platform(ctx, func(transaction *sql.Tx) error {
			userIDs, err := postgres.PrunableUsers(ctx, transaction, retention.options.Retention, retention.options.BatchSize)
			if err != nil {
				return err
			}
			for _, userID := range userIDs {
				boundary, err := postgres.PruneUserEvents(ctx, transaction, userID, retention.options.Retention, retention.options.BatchSize)
				if err != nil {
					// 整批回滚：边界与删除必须一起生效，部分推进会让游标判断失真。
					return err
				}
				if boundary > 0 {
					pruned++
					retention.logger.DebugContext(ctx, "已清理用户历史事件", "user_id", userID, "pruned_through", boundary)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if pruned == 0 {
			return nil
		}
	}
	return nil
}
