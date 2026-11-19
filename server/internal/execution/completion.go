package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
)

// Complete 原子保存运行终态、可见回复及事件，仍有未结算外部操作时拒绝释放任务。
// 成功必须有已绑定模型的成功操作；取消中的运行只能按取消结果收尾。
func (service *Service) Complete(ctx context.Context, lease Lease, outcome Outcome) (Run, error) {
	if !isTerminal(outcome.Status) || (outcome.Answer != "" && !validText(outcome.Answer, 64<<10)) ||
		(outcome.Status == "succeeded" && outcome.ErrorCode != "") || (outcome.Status != "succeeded" && !codePattern.MatchString(outcome.ErrorCode)) {
		return Run{}, ErrInvalidInput
	}
	var run Run
	err := service.worker(ctx, lease, true, func(transaction *sql.Tx) error {
		if pending, err := computerPending(ctx, transaction, lease); err != nil {
			return err
		} else if pending {
			return ErrComputerBusy
		}
		if outcome.Status == "succeeded" || outcome.ErrorCode == "COMPUTER_LEASE_LOST" {
			lossCode, err := computerLossCode(ctx, transaction, lease)
			if err != nil {
				return err
			}
			if lossCode != "" {
				if outcome.Status == "succeeded" {
					return ErrComputerLost
				}
				outcome.ErrorCode = lossCode
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
		var isConsole bool
		if err = transaction.QueryRowContext(ctx, `SELECT source='console' FROM tombot.turns WHERE user_id=$1 AND id=$2`, lease.UserID, lease.TurnID).Scan(&isConsole); err != nil {
			return err
		}
		if outcome.Status == "succeeded" && isConsole {
			var confirmed bool
			if outcome.Answer != "" {
				return ErrInvalidInput
			}
			if err = transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.desktop_consoles WHERE user_id=$1 AND run_id=$2 AND state='closed') AND NOT EXISTS(SELECT 1 FROM tombot.desktop_consoles WHERE user_id=$1 AND run_id=$2 AND state<>'closed')`, lease.UserID, lease.RunID).Scan(&confirmed); err != nil {
				return err
			}
			if !confirmed {
				return ErrUncertain
			}
		} else if outcome.Status == "succeeded" && outcome.Answer == "" {
			return ErrInvalidInput
		}
		var started, unknown, modelSucceeded bool
		if err = transaction.QueryRowContext(ctx, `SELECT COALESCE(bool_or(status='started'),false),COALESCE(bool_or(status='unknown'),false),COALESCE(bool_or(kind='model' AND status='succeeded'),false)
			FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID).Scan(&started, &unknown, &modelSucceeded); err != nil {
			return err
		}
		if started || outcome.Status == "succeeded" && (unknown || !modelSucceeded && !isConsole) {
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
	group, err := isGroupRun(ctx, transaction, lease)
	if err != nil {
		return err
	}
	var private bool
	if err = transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.bot_private_runs WHERE user_id=$1 AND run_id=$2)`, lease.UserID, lease.RunID).Scan(&private); err != nil {
		return err
	}
	if group || private {
		if err = lockDispatch(ctx, transaction, lease.UserID); err != nil {
			return err
		}
	}
	// 与消息受理共用会话锁；最终模型调用期间到达的消息会原子转入后续运行。
	var conversationID string
	if err := transaction.QueryRowContext(ctx, `SELECT id FROM tombot.conversations WHERE user_id=$1 AND id=$2 FOR UPDATE`, lease.UserID, lease.ConversationID).Scan(&conversationID); err != nil {
		return err
	}
	var pending bool
	if err := transaction.QueryRowContext(ctx, `SELECT context_sequence IS NOT NULL AND EXISTS(SELECT 1 FROM tombot.run_inputs i JOIN tombot.messages m ON m.user_id=i.user_id AND m.id=i.message_id WHERE i.user_id=r.user_id AND i.turn_id=r.turn_id AND m.sequence>r.input_sequence) FROM tombot.runs r WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID).Scan(&pending); err != nil {
		return err
	}
	if group {
		if err = available(ctx, transaction, lease); errors.Is(err, ErrUnavailable) {
			outcome = Outcome{Status: "failed", ErrorCode: "RESOURCE_UNAVAILABLE"}
		} else if err != nil {
			return err
		}
		if outcome.Status != "succeeded" && outcome.Answer == "" {
			var name string
			if err = transaction.QueryRowContext(ctx, `SELECT bot_snapshot->>'name' FROM tombot.runs WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID).Scan(&name); err != nil {
				return err
			}
			reason := "本次未能完成回复，请稍后重试。"
			if outcome.ErrorCode == "RESOURCE_UNAVAILABLE" {
				reason = "本次未能完成回复，请检查群成员和猫咪状态。"
			}
			if outcome.Status == "canceled" {
				reason = "本次回复已取消。"
			}
			if err = writeGroupMessage(ctx, transaction, lease.UserID, lease.ConversationID, lease.TurnID, lease.RunID, "", ids.New(), "", []Block{{Kind: "text", Text: "「" + name + "」" + reason}}, "complete"); err != nil {
				return err
			}
		}
	}
	if private && !group {
		if err = available(ctx, transaction, lease); errors.Is(err, ErrUnavailable) {
			outcome = Outcome{Status: "failed", ErrorCode: "RESOURCE_UNAVAILABLE"}
		} else if err != nil {
			return err
		}
	}
	if group && outcome.Answer != "" {
		if err = finishGroupAnswer(ctx, transaction, lease, outcome); err != nil {
			return err
		}
	}
	if outcome.Answer != "" && !group {
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
	if _, err := transaction.ExecContext(ctx, `UPDATE tombot.runs SET status=$3,error_code=$4,finished_at=clock_timestamp(),accepts_console=false WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID, outcome.Status, outcome.ErrorCode); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE tombot.desktop_consoles SET state='failed',error_code='RUN_ENDED' WHERE user_id=$1 AND run_id=$2 AND state IN ('waiting','pausing','active','resuming')`, lease.UserID, lease.RunID); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE tombot.jobs SET status='finished',lease_owner=NULL,lease_expires_at=NULL,fencing_token=fencing_token+1,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID); err != nil {
		return err
	}
	if pending {
		next := ids.New()
		if _, err := transaction.ExecContext(ctx, `INSERT INTO tombot.runs(user_id,id,turn_id,conversation_id,bot_id,bot_revision,bot_snapshot,status,continuation_after) SELECT user_id,$3,turn_id,conversation_id,bot_id,bot_revision,bot_snapshot,'queued',input_sequence FROM tombot.runs WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID, next); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `INSERT INTO tombot.jobs(user_id,id,run_id,bot_id,conversation_id,turn_id,status) VALUES($1,$2,$3,$4,$5,$6,'queued')`, lease.UserID, ids.New(), next, lease.BotID, lease.ConversationID, lease.TurnID); err != nil {
			return err
		}
		if err := event(ctx, transaction, lease.UserID, "run.queued", next, map[string]string{"bot_id": lease.BotID, "conversation_id": lease.ConversationID, "turn_id": lease.TurnID}); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `INSERT INTO tombot.bot_private_runs(user_id,run_id,chain_id,response_to,path,trigger_message_id,conversation_id) SELECT user_id,$3,chain_id,response_to,path,trigger_message_id,conversation_id FROM tombot.bot_private_runs WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID, next); err != nil {
			return err
		}
	}
	if group && outcome.Status != "canceled" && outcome.ErrorCode != "RESOURCE_UNAVAILABLE" {
		if err = followGroupMentions(ctx, transaction, lease); err != nil {
			return err
		}
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
	if err := event(ctx, transaction, lease.UserID, "run.finished", lease.RunID, map[string]string{"status": outcome.Status, "error_code": outcome.ErrorCode, "bot_id": lease.BotID, "conversation_id": lease.ConversationID}); err != nil {
		return err
	}
	if err := stagePrivateReply(ctx, transaction, lease, outcome); err != nil {
		return err
	}
	return stageTaskResult(ctx, transaction, lease, outcome)
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
