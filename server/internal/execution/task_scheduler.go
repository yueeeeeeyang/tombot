package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/scheduledtasks"
)

type dueTask struct{ user, id string }

// DispatchTasks 每轮有界读取到期索引；同一用户受理锁与任务行锁将抢占、入队及推进合为一次事务。
func (service *Service) DispatchTasks(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, ErrInvalidInput
	}
	due := []dueTask{}
	err := service.dispatch.Platform(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT user_id,task_id FROM tombot.scheduled_task_due WHERE next_run_at<=clock_timestamp() ORDER BY next_run_at,user_id,task_id LIMIT $1`, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item dueTask
			if err = rows.Scan(&item.user, &item.id); err != nil {
				return errors.Join(err, rows.Close())
			}
			due = append(due, item)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return 0, translate(err)
	}
	count := 0
	var failures error
	for _, item := range due {
		dispatched := false
		err = service.tenant.Tenant(ctx, item.user, func(tx *sql.Tx) error {
			if err := lockDispatch(ctx, tx, item.user); err != nil {
				return err
			}
			var bot string
			err := tx.QueryRowContext(ctx, `SELECT bot_id FROM tombot.scheduled_tasks WHERE user_id=$1 AND id=$2 AND NOT is_deleted AND is_enabled AND next_run_at<=clock_timestamp() FOR UPDATE`, item.user, item.id).Scan(&bot)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			task, err := scheduledtasks.Read(ctx, tx, item.user, bot, item.id, false)
			if err != nil {
				return err
			}
			var now time.Time
			if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
				return err
			}
			scheduled := task.NextRunAt
			definition := task.Definition
			if definition.Cron == "" {
				definition.IsEnabled = false
			}
			next, err := definition.Next(now)
			if err != nil {
				return err
			}
			status, code := "queued", ""
			var submitted Submission
			// 入队失败保留可见记录，存储或事件错误则回滚整次触发，不提前推进时间。
			if _, err = tx.ExecContext(ctx, `SAVEPOINT scheduled_enqueue`); err != nil {
				return err
			}
			var active bool
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.users WHERE id=$1 AND status='active')`, item.user).Scan(&active)
			if err != nil {
				return err
			}

			if !active {
				err = ErrUnavailable
			} else {
				var parentBot string
				parentBot, err = lockConversation(ctx, tx, item.user, task.ConversationID)
				if err == nil && parentBot != bot {
					err = ErrUnavailable
				}
				if err == nil {
					conversation := ids.New()
					_, err = tx.ExecContext(ctx, `INSERT INTO tombot.conversations(user_id,id,kind,bot_id,title) VALUES($1,$2,'task',$3,'后台任务')`, item.user, conversation, bot)
					if err == nil {
						err = service.submitInput(ctx, tx, item.user, conversation, SendRequest{ClientRequestID: ids.New(), Content: "任务：" + task.Name + "\n" + task.Prompt}, nil, "routine", &submitted)
					}
				}
			}
			if err != nil {
				switch {
				case errors.Is(err, ErrCapacity):
					return err
				case errors.Is(err, ErrUnavailable), errors.Is(err, ErrConversationKind), errors.Is(err, sql.ErrNoRows):
					status, code = "failed", "TARGET_UNAVAILABLE"
					definition.IsEnabled = false
					next = nil
				default:
					return err
				}
				if _, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT scheduled_enqueue`); err != nil {
					return err
				}
			}
			if _, err = tx.ExecContext(ctx, `RELEASE SAVEPOINT scheduled_enqueue`); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.scheduled_task_runs(user_id,task_id,scheduled_at,task_revision,run_id,status,error_code) VALUES($1,$2,$3,$4,NULLIF($5,''),$6,$7)`, item.user, item.id, scheduled, task.Revision, submitted.RunID, status, code); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE tombot.scheduled_tasks SET next_run_at=$3,is_enabled=$4,revision=revision+CASE WHEN is_enabled<>$4 THEN 1 ELSE 0 END,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, item.user, item.id, next, definition.IsEnabled); err != nil {
				return err
			}
			if err = scheduledtasks.SyncDue(ctx, tx, item.user, item.id, next); err != nil {
				return err
			}
			if err = scheduledtasks.Changed(ctx, tx, item.user, bot, item.id); err != nil {
				return err
			}
			dispatched = true
			return nil
		})
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("任务调度失败（用户 %s，任务 %s）：%w", item.user, item.id, translate(err)))
			continue
		}
		if dispatched {
			count++
		}
	}
	return count, failures
}

// scheduleTasks 独立于模型运行轮询，客户端离线时仍工作；每轮有时限，失败后按固定间隔再核对数据库。
func (worker *Worker) scheduleTasks(ctx context.Context) {
	for ctx.Err() == nil {
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := worker.executor.DispatchTasks(bounded, 100)
		_, deliveryErr := worker.executor.DeliverTaskResults(bounded, 100)
		err = errors.Join(err, deliveryErr)
		cancel()
		if err != nil && ctx.Err() == nil {
			worker.logger.ErrorContext(ctx, "定时任务调度失败，等待下一轮核对", "reason", err)
		}
		if !waitPoll(ctx, time.Second) {
			return
		}
	}
}
