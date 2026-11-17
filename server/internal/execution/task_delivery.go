package execution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/scheduledtasks"
)

// stageTaskResult 与后台运行收尾共享事务，保存可重启的结果投递意图，不提前写入正在进行的聊天。
func stageTaskResult(ctx context.Context, tx *sql.Tx, lease Lease, outcome Outcome) error {
	var task, conversation string
	err := tx.QueryRowContext(ctx, `SELECT h.task_id,t.conversation_id FROM tombot.scheduled_task_runs h JOIN tombot.scheduled_tasks t ON t.user_id=h.user_id AND t.id=h.task_id WHERE h.user_id=$1 AND h.run_id=$2`, lease.UserID, lease.RunID).Scan(&task, &conversation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tombot.scheduled_task_runs SET result_text=$3,delivery_status='waiting' WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID, outcome.Answer); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.scheduled_task_delivery(user_id,run_id,task_id,conversation_id) VALUES($1,$2,$3,$4)`, lease.UserID, lease.RunID, task, conversation); err != nil {
		return err
	}
	return scheduledtasks.Changed(ctx, tx, lease.UserID, lease.BotID, task)
}

type taskDelivery struct{ user, run, conversation string }

const unavailableTaskDeliveryDelay = time.Minute

// DeliverTaskResults 只投递已经完成的任务；与聊天受理共用短事务锁，忙碌时保留结果且不重复调用模型。
func (service *Service) DeliverTaskResults(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, ErrInvalidInput
	}
	pending := []taskDelivery{}
	err := service.dispatch.Platform(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT d.user_id,d.run_id,d.conversation_id FROM tombot.scheduled_task_delivery d WHERE d.available_at<=clock_timestamp() AND NOT EXISTS(SELECT 1 FROM tombot.jobs j WHERE j.user_id=d.user_id AND j.conversation_id=d.conversation_id AND j.status<>'finished') ORDER BY d.available_at,d.ready_at,d.user_id,d.run_id LIMIT $1`, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item taskDelivery
			if err = rows.Scan(&item.user, &item.run, &item.conversation); err != nil {
				return errors.Join(err, rows.Close())
			}
			pending = append(pending, item)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return 0, translate(err)
	}
	count := 0
	var failures error
	for _, item := range pending {
		delivered := false
		err = service.tenant.Tenant(ctx, item.user, func(tx *sql.Tx) error {
			if err := lockDispatch(ctx, tx, item.user); err != nil {
				return err
			}
			var bot string
			err := tx.QueryRowContext(ctx, `SELECT c.bot_id FROM tombot.conversations c JOIN tombot.bots b ON b.user_id=c.user_id AND b.id=c.bot_id JOIN tombot.users u ON u.id=c.user_id WHERE c.user_id=$1 AND c.id=$2 AND NOT c.is_archived AND NOT b.is_archived AND u.status='active' FOR UPDATE OF c`, item.user, item.conversation).Scan(&bot)
			if errors.Is(err, sql.ErrNoRows) {
				// 不可用会话暂缓核对，避免占满批次使其他猫咪的结果饥饿。
				_, err = tx.ExecContext(ctx, `UPDATE tombot.scheduled_task_delivery SET available_at=clock_timestamp()+$3::bigint*interval '1 millisecond' WHERE user_id=$1 AND run_id=$2`, item.user, item.run, unavailableTaskDeliveryDelay.Milliseconds())
				return err
			}
			if err != nil {
				return err
			}
			var busy bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.turns WHERE user_id=$1 AND conversation_id=$2 AND status IN ('queued','running','canceling'))`, item.user, item.conversation).Scan(&busy); err != nil {
				return err
			}
			if busy {
				return nil
			}
			var task, name, answer, status string
			err = tx.QueryRowContext(ctx, `SELECT h.task_id,t.name,h.result_text,r.status FROM tombot.scheduled_task_delivery d JOIN tombot.scheduled_task_runs h ON h.user_id=d.user_id AND h.run_id=d.run_id JOIN tombot.scheduled_tasks t ON t.user_id=h.user_id AND t.id=h.task_id JOIN tombot.runs r ON r.user_id=d.user_id AND r.id=d.run_id WHERE d.user_id=$1 AND d.run_id=$2 FOR UPDATE OF d`, item.user, item.run).Scan(&task, &name, &answer, &status)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if !isTerminal(status) {
				return ErrConflict
			}
			content := "任务「" + name + "」\n" + answer
			if status != "succeeded" {
				content = "任务「" + name + "」未完成。"
				if answer != "" {
					content += "\n" + answer
				}
			}
			encoded, err := json.Marshal([]Block{{Kind: "text", Text: content, TaskResult: true}})
			if err != nil {
				return err
			}
			var turnSequence, messageSequence int64
			if err = tx.QueryRowContext(ctx, `UPDATE tombot.conversations SET turn_sequence=turn_sequence+1,message_sequence=message_sequence+1,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2 RETURNING turn_sequence,message_sequence`, item.user, item.conversation).Scan(&turnSequence, &messageSequence); err != nil {
				return err
			}
			turn, message := ids.New(), ids.New()
			digest := sha256.Sum256([]byte(item.run))
			if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.turns(user_id,id,conversation_id,sequence,client_request_id,request_digest,source,status,finished_at) VALUES($1,$2,$3,$4,$5,$6,'routine','succeeded',clock_timestamp())`, item.user, turn, item.conversation, turnSequence, ids.New(), digest[:]); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.messages(user_id,id,conversation_id,turn_id,sequence,sender_kind,blocks,status) VALUES($1,$2,$3,$4,$5,'system',$6,'complete')`, item.user, message, item.conversation, turn, messageSequence, encoded); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE tombot.scheduled_task_runs SET delivery_status='delivered',delivery_message_id=$3 WHERE user_id=$1 AND run_id=$2`, item.user, item.run, message); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM tombot.scheduled_task_delivery WHERE user_id=$1 AND run_id=$2`, item.user, item.run); err != nil {
				return err
			}
			if err = event(ctx, tx, item.user, "message.created", message, map[string]any{"conversation_id": item.conversation, "sequence": messageSequence, "turn_id": turn}); err != nil {
				return err
			}
			if err = scheduledtasks.Changed(ctx, tx, item.user, bot, task); err != nil {
				return err
			}
			delivered = true
			return nil
		})
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("任务结果投递失败（用户 %s，运行 %s）：%w", item.user, item.run, translate(err)))
			continue
		}
		if delivered {
			count++
		}
	}
	return count, failures
}
