package scheduledtasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
)

func ValidID(id string) bool { _, err := ulid.ParseStrict(id); return err == nil }

type scanner interface{ Scan(...any) error }

const columns = `COALESCE((SELECT delivery_status FROM tombot.scheduled_task_runs WHERE user_id=t.user_id AND task_id=t.id ORDER BY scheduled_at DESC LIMIT 1),''),t.id,t.bot_id,t.conversation_id,t.name,t.prompt,t.cron,t.timezone,t.run_at,t.is_enabled,t.revision,t.next_run_at,
 (SELECT scheduled_at FROM tombot.scheduled_task_runs WHERE user_id=t.user_id AND task_id=t.id ORDER BY scheduled_at DESC LIMIT 1),
 (SELECT run_id FROM tombot.scheduled_task_runs WHERE user_id=t.user_id AND task_id=t.id ORDER BY scheduled_at DESC LIMIT 1),
 COALESCE((SELECT COALESCE(r.status,h.status) FROM tombot.scheduled_task_runs h LEFT JOIN tombot.runs r ON r.user_id=h.user_id AND r.id=h.run_id WHERE h.user_id=t.user_id AND h.task_id=t.id ORDER BY h.scheduled_at DESC LIMIT 1),''),
 COALESCE((SELECT COALESCE(r.error_code,h.error_code) FROM tombot.scheduled_task_runs h LEFT JOIN tombot.runs r ON r.user_id=h.user_id AND r.id=h.run_id WHERE h.user_id=t.user_id AND h.task_id=t.id ORDER BY h.scheduled_at DESC LIMIT 1),'')`

func scan(row scanner) (Task, error) {
	var task Task
	err := row.Scan(&task.LastDeliveryStatus, &task.ID, &task.BotID, &task.ConversationID, &task.Name, &task.Prompt, &task.Cron, &task.Timezone, &task.RunAt, &task.IsEnabled, &task.Revision, &task.NextRunAt, &task.LastRunAt, &task.LastRunID, &task.LastStatus, &task.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return task, err
}

// Read 可选择加行锁，所有读写都绑定所属用户和猫咪，删除项不再对外暴露。
func Read(ctx context.Context, tx *sql.Tx, user, bot, id string, lock bool) (Task, error) {
	if !ValidID(id) {
		return Task{}, ErrInput
	}
	suffix := ""
	if lock {
		suffix = " FOR UPDATE OF t"
	}
	return scan(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM tombot.scheduled_tasks t WHERE t.user_id=$1 AND t.bot_id=$2 AND t.id=$3 AND NOT t.is_deleted`+suffix, user, bot, id))
}

// List 使用稳定 ID 游标分页；每只猫咪的上限使读取和容量检查有界。
func List(ctx context.Context, tx *sql.Tx, user, bot, before string, limit int) (page Page, failure error) {
	if limit < 1 || limit > PageSize || !ValidID(bot) || before != "" && !ValidID(before) {
		return page, ErrInput
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.bots WHERE user_id=$1 AND id=$2)`, user, bot).Scan(&exists); err != nil {
		return page, err
	}
	if !exists {
		return page, ErrNotFound
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+columns+` FROM tombot.scheduled_tasks t WHERE t.user_id=$1 AND t.bot_id=$2 AND NOT t.is_deleted AND ($3='' OR t.id<$3) ORDER BY t.id DESC LIMIT $4`, user, bot, before, limit+1)
	if err != nil {
		return page, err
	}
	defer func() { failure = errors.Join(failure, rows.Close()) }()
	page.Tasks = []Task{}
	for rows.Next() {
		task, err := scan(rows)
		if err != nil {
			return page, err
		}
		page.Tasks = append(page.Tasks, task)
	}
	if len(page.Tasks) > limit {
		page.Tasks = page.Tasks[:limit]
		page.NextBefore = page.Tasks[limit-1].ID
	}
	return page, rows.Err()
}

// Save 在调用方事务内更新定义、到期索引和事件；版本冲突或事件失败均整体回滚。
func Save(ctx context.Context, tx *sql.Tx, user, bot, id string, revision int64, definition Definition) (Task, error) {
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return Task{}, err
	}
	next, err := definition.Next(now)
	if err != nil {
		return Task{}, err
	}
	if !ValidID(bot) || revision < 0 || (id == "") != (revision == 0) {
		return Task{}, ErrInput
	}
	var conversation string
	// 创建容量检查与同一猫咪其他创建串行；不锁运行队列，避免与工具租约形成反向锁序。
	if err = tx.QueryRowContext(ctx, `SELECT c.id FROM tombot.bots b JOIN tombot.conversations c ON c.user_id=b.user_id AND c.bot_id=b.id AND c.kind='direct' WHERE b.user_id=$1 AND b.id=$2 AND NOT b.is_archived AND NOT c.is_archived FOR UPDATE OF b`, user, bot).Scan(&conversation); errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	} else if err != nil {
		return Task{}, err
	}
	if id == "" {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tombot.scheduled_tasks WHERE user_id=$1 AND bot_id=$2 AND NOT is_deleted`, user, bot).Scan(&count); err != nil {
			return Task{}, err
		}
		if count >= MaxTasks {
			return Task{}, ErrCapacity
		}
		id = ids.New()
		_, err = tx.ExecContext(ctx, `INSERT INTO tombot.scheduled_tasks(user_id,id,bot_id,conversation_id,name,prompt,cron,timezone,run_at,is_enabled,next_run_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, user, id, bot, conversation, definition.Name, definition.Prompt, definition.Cron, definition.Timezone, definition.RunAt, definition.IsEnabled, next)
	} else {
		current, readErr := Read(ctx, tx, user, bot, id, true)
		if readErr != nil {
			return Task{}, readErr
		}
		if current.Revision != revision {
			return Task{}, ErrConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE tombot.scheduled_tasks SET name=$4,prompt=$5,cron=$6,timezone=$7,run_at=$8,is_enabled=$9,next_run_at=$10,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND bot_id=$2 AND id=$3`, user, bot, id, definition.Name, definition.Prompt, definition.Cron, definition.Timezone, definition.RunAt, definition.IsEnabled, next)
	}
	if err != nil {
		return Task{}, err
	}
	if err = SyncDue(ctx, tx, user, id, next); err != nil {
		return Task{}, err
	}
	if err = Changed(ctx, tx, user, bot, id); err != nil {
		return Task{}, err
	}
	return Read(ctx, tx, user, bot, id, false)
}

// Delete 保留执行审计但移除未来计划；已经入队的运行不被悄悄取消。
func Delete(ctx context.Context, tx *sql.Tx, user, bot, id string, revision int64) error {
	if revision < 1 {
		return ErrInput
	}
	task, err := Read(ctx, tx, user, bot, id, true)
	if err != nil {
		return err
	}
	if task.Revision != revision {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tombot.scheduled_tasks SET is_deleted=true,is_enabled=false,next_run_at=NULL,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, user, id); err != nil {
		return err
	}
	if err = SyncDue(ctx, tx, user, id, nil); err != nil {
		return err
	}
	return Changed(ctx, tx, user, bot, id)
}

// SyncDue 与定义共享事务，调度连接始终只读索引，无法获取用户正文。
func SyncDue(ctx context.Context, tx *sql.Tx, user, id string, next *time.Time) error {
	if next == nil {
		_, err := tx.ExecContext(ctx, `DELETE FROM tombot.scheduled_task_due WHERE user_id=$1 AND task_id=$2`, user, id)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO tombot.scheduled_task_due(user_id,task_id,next_run_at) VALUES($1,$2,$3) ON CONFLICT(user_id,task_id) DO UPDATE SET next_run_at=EXCLUDED.next_run_at`, user, id, next)
	return err
}

// Changed 只广播失效提示，不把任务内容写入事件正文。
func Changed(ctx context.Context, tx *sql.Tx, user, bot, id string) error {
	payload, err := json.Marshal(map[string]string{"bot_id": bot})
	if err != nil {
		return err
	}
	_, err = postgres.AppendEvent(ctx, tx, user, "task.updated", id, payload)
	return err
}
