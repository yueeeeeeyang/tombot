package execution

import (
	"context"
	"database/sql"
	"encoding/json"

	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
)

// Complete 原子保存运行终态、可见回复及事件，仍有未结算外部操作时拒绝释放任务。
// 成功必须有已绑定模型的成功操作；取消中的运行只能按取消结果收尾。
func (service *Service) Complete(ctx context.Context, lease Lease, outcome Outcome) (Run, error) {
	if !isTerminal(outcome.Status) || (outcome.Answer != "" && !validText(outcome.Answer, 64<<10)) ||
		(outcome.Status == "succeeded" && (outcome.ErrorCode != "" || outcome.Answer == "")) || (outcome.Status != "succeeded" && !codePattern.MatchString(outcome.ErrorCode)) {
		return Run{}, ErrInvalidInput
	}
	var run Run
	err := service.worker(ctx, lease, true, func(transaction *sql.Tx) error {
		if pending, err := computerPending(ctx, transaction, lease); err != nil {
			return err
		} else if pending {
			return ErrComputerBusy
		}
		if outcome.Status == "succeeded" {
			lost, err := computerLost(ctx, transaction, lease)
			if err != nil {
				return err
			}
			if lost {
				return ErrComputerLost
			}
		}
		var err error
		run, err = readRun(ctx, transaction, lease.UserID, lease.RunID)
		if err != nil {
			return err
		}
		if run.Status == "canceling" && outcome.Status != "canceled" {
			return ErrCanceled
		}
		if run.Status != "running" && run.Status != "canceling" && !(run.Status == "queued" && outcome.Status != "succeeded") {
			return ErrConflict
		}
		var started, unknown, modelSucceeded bool
		if err = transaction.QueryRowContext(ctx, `SELECT COALESCE(bool_or(status='started'),false),COALESCE(bool_or(status='unknown'),false),COALESCE(bool_or(kind='model' AND status='succeeded'),false)
			FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID).Scan(&started, &unknown, &modelSucceeded); err != nil {
			return err
		}
		if started || outcome.Status == "succeeded" && (unknown || !modelSucceeded) {
			return ErrUncertain
		}
		if err = finishRun(ctx, transaction, lease, outcome); err != nil {
			return err
		}
		run, err = readRun(ctx, transaction, lease.UserID, lease.RunID)
		return err
	})
	if err != nil {
		return Run{}, err
	}
	return run, nil
}

// finishRun 的调用者必须已锁定队列记录；恢复器也使用相同的终态与事件实现。
func finishRun(ctx context.Context, transaction *sql.Tx, lease Lease, outcome Outcome) error {
	if outcome.Answer != "" {
		var sequence int64
		if err := transaction.QueryRowContext(ctx, `UPDATE tombot.conversations SET message_sequence=message_sequence+1,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2 RETURNING message_sequence`, lease.UserID, lease.ConversationID).Scan(&sequence); err != nil {
			return err
		}
		blocks, err := json.Marshal([]Block{{Kind: "text", Text: outcome.Answer}})
		if err != nil {
			return err
		}
		status := "complete"
		if outcome.Status != "succeeded" {
			status = "interrupted"
		}
		id := ids.New()
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.messages(user_id,id,conversation_id,turn_id,run_id,sequence,sender_kind,sender_bot_id,blocks,status)
			VALUES($1,$2,$3,$4,$5,$6,'bot',$7,$8,$9)`, lease.UserID, id, lease.ConversationID, lease.TurnID, lease.RunID, sequence, lease.BotID, blocks, status); err != nil {
			return err
		}
		if err = event(ctx, transaction, lease.UserID, "message.created", id, map[string]any{"conversation_id": lease.ConversationID, "sequence": sequence, "turn_id": lease.TurnID}); err != nil {
			return err
		}
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE tombot.runs SET status=$3,error_code=$4,finished_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID, outcome.Status, outcome.ErrorCode); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE tombot.jobs SET status='finished',lease_owner=NULL,lease_expires_at=NULL,fencing_token=fencing_token+1,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID); err != nil {
		return err
	}
	var active, unknown, failed, canceled bool
	if err := transaction.QueryRowContext(ctx, `SELECT bool_or(status IN ('queued','running','canceling')),bool_or(status='unknown'),bool_or(status='failed'),bool_or(status='canceled')
		FROM tombot.runs WHERE user_id=$1 AND turn_id=$2`, lease.UserID, lease.TurnID).Scan(&active, &unknown, &failed, &canceled); err != nil {
		return err
	}
	if !active {
		status, code := "succeeded", ""
		switch {
		case unknown:
			status, code = "unknown", "EXTERNAL_RESULT_UNKNOWN"
		case failed:
			status, code = "failed", "RUN_FAILED"
		case canceled:
			status, code = "canceled", "CANCELED"
		}
		if status == outcome.Status {
			code = outcome.ErrorCode
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE tombot.turns SET status=$3,error_code=$4,finished_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, lease.UserID, lease.TurnID, status, code); err != nil {
			return err
		}
		if err := event(ctx, transaction, lease.UserID, "turn.finished", lease.TurnID, map[string]string{"status": status, "error_code": code}); err != nil {
			return err
		}
	}
	return event(ctx, transaction, lease.UserID, "run.finished", lease.RunID, map[string]string{"status": outcome.Status, "error_code": outcome.ErrorCode})
}

// Cancel 排队任务立即终止；已领取任务持久请求取消，等待持有者或恢复器收尾。
// 不直接释放仍可能有外部进程或模型请求的租约。
func (service *Service) Cancel(ctx context.Context, principal identity.Principal, runID string) (Run, error) {
	if !validID(runID) {
		return Run{}, ErrInvalidInput
	}
	var run Run
	err := service.user(ctx, principal, true, func(transaction *sql.Tx) error {
		lease := Lease{UserID: principal.UserID, RunID: runID}
		var status string
		if err := transaction.QueryRowContext(ctx, `SELECT id,bot_id,conversation_id,turn_id,status FROM tombot.jobs WHERE user_id=$1 AND run_id=$2 FOR UPDATE`, principal.UserID, runID).Scan(&lease.JobID, &lease.BotID, &lease.ConversationID, &lease.TurnID, &status); err != nil {
			return err
		}
		var err error
		run, err = readRun(ctx, transaction, principal.UserID, runID)
		if err != nil {
			return err
		}
		if isTerminal(run.Status) || run.Status == "canceling" {
			return nil
		}
		if status == "queued" {
			if err = finishRun(ctx, transaction, lease, Outcome{Status: "canceled", ErrorCode: "CANCELED"}); err != nil {
				return err
			}
		} else if status == "leased" {
			if _, err = transaction.ExecContext(ctx, `UPDATE tombot.jobs SET cancel_requested_at=clock_timestamp(),updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID); err != nil {
				return err
			}
			if _, err = transaction.ExecContext(ctx, `UPDATE tombot.runs SET status='canceling' WHERE user_id=$1 AND id=$2`, lease.UserID, runID); err != nil {
				return err
			}
			if _, err = transaction.ExecContext(ctx, `UPDATE tombot.turns SET status='canceling' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.TurnID); err != nil {
				return err
			}
			if err = event(ctx, transaction, lease.UserID, "run.cancel_requested", runID, struct{}{}); err != nil {
				return err
			}
		} else {
			return ErrConflict
		}
		run, err = readRun(ctx, transaction, principal.UserID, runID)
		return err
	})
	if err != nil {
		return Run{}, err
	}
	return run, nil
}
