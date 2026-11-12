package execution

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io.yueyang/tom/internal/bots"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelusage"
)

const operationColumns = `id,sequence,step_key,kind,status,result,error_code,attempt,fencing_token,request_digest`

func scanOperation(row scanner) (Operation, error) {
	var operation Operation
	err := row.Scan(&operation.ID, &operation.Sequence, &operation.StepKey, &operation.Kind, &operation.Status, &operation.Result, &operation.ErrorCode, &operation.Attempt, &operation.FencingToken, &operation.RequestDigest)
	return operation, err
}
func readOperations(ctx context.Context, transaction *sql.Tx, userID, runID string, limit int) (operations []Operation, result error) {
	rows, err := transaction.QueryContext(ctx, `SELECT `+operationColumns+` FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2 ORDER BY started_at,id LIMIT $3`, userID, runID, limit+1)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, rows.Close()) }()
	operations = make([]Operation, 0, limit)
	for rows.Next() {
		operation, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, operation)
	}
	if len(operations) > limit {
		return nil, ErrOperationLimit
	}
	return operations, rows.Err()
}

// BeginOperation 先持久记账再允许外部动作，只有返回 isNew=true 时才能执行一次。
// 重复步骤返回原记录，started 或 unknown 不等于可重试；成功记录应直接复用结果。
func (service *Service) BeginOperation(ctx context.Context, lease Lease, request OperationRequest) (Operation, bool, error) {
	if !stepPattern.MatchString(request.StepKey) || (request.Kind != "model" && request.Kind != "tool" && request.Kind != "computer") || len(request.RequestDigest) != 32 {
		return Operation{}, false, ErrInvalidInput
	}
	if (request.Kind == "model") != (request.Model != nil) {
		return Operation{}, false, ErrInvalidInput
	}
	if (request.Kind == "tool" || request.Kind == "computer") && !bots.ValidToolName(request.ToolName) {
		return Operation{}, false, ErrInvalidInput
	}
	if request.Kind == "computer" && request.Computer == nil {
		return Operation{}, false, ErrInvalidInput
	}
	var computerID *string
	if request.Computer != nil {
		if request.Computer.Validate() != nil || request.Computer.UserID != lease.UserID || request.Computer.RunID != lease.RunID || request.Computer.BotID != lease.BotID {
			return Operation{}, false, ErrInvalidInput
		}
		if request.Kind == "computer" {
			computerID = &request.Computer.SessionID
		}
	}
	var operation Operation
	created := false
	err := service.worker(ctx, lease, false, func(transaction *sql.Tx) error {
		if err := available(ctx, transaction, lease); err != nil {
			return err
		}
		if request.Kind == "tool" || request.Kind == "computer" {
			var permitted bool
			if err := transaction.QueryRowContext(ctx, `SELECT tool_permissions ? $3 FROM tombot.bots WHERE user_id=$1 AND id=$2`, lease.UserID, lease.BotID, request.ToolName).Scan(&permitted); err != nil {
				return err
			}
			if !permitted {
				return ErrUnavailable
			}
		}
		var status string
		var isBound bool
		if err := transaction.QueryRowContext(ctx, `SELECT status,model_policy_id IS NOT NULL FROM tombot.runs WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID).Scan(&status, &isBound); err != nil {
			return err
		}
		if status != "running" || request.Kind == "model" && !isBound {
			return ErrConflict
		}
		if session := request.Computer; session != nil {
			var active bool
			if err := transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.computer_sessions WHERE user_id=$1 AND id=$2 AND run_id=$3 AND job_owner=$4 AND job_fencing_token=$5 AND job_attempt=$6
				AND computer_id=$7 AND node_id=$8 AND container_id=$9 AND generation=$10 AND fencing_token=$11 AND desktop_id=$12 AND linux_uid=$13 AND shared_gid=$14 AND display_number=$15
				AND status='active' AND lease_expires_at>clock_timestamp() AND hold_deadline_at>clock_timestamp())`,
				lease.UserID, session.SessionID, lease.RunID, lease.OwnerID, lease.FencingToken, lease.Attempt, session.ComputerID, session.NodeID, session.ContainerID, session.Generation, session.FencingToken, session.DesktopID, session.UID, session.GID, session.DisplayNumber).Scan(&active); err != nil {
				return err
			}
			if !active {
				return ErrLeaseLost
			}
		}
		var digest []byte
		err := transaction.QueryRowContext(ctx, `SELECT request_digest FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2 AND step_key=$3`, lease.UserID, lease.RunID, request.StepKey).Scan(&digest)
		if err == nil {
			operation, err = scanOperation(transaction.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2 AND step_key=$3`, lease.UserID, lease.RunID, request.StepKey))
			if err != nil {
				return err
			}
			if !bytes.Equal(digest, request.RequestDigest) || operation.Kind != request.Kind {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var count int
		var inFlight bool
		if err = transaction.QueryRowContext(ctx, `SELECT count(*),COALESCE(bool_or(status IN ('started','unknown')),false) FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID).Scan(&count, &inFlight); err != nil {
			return err
		}
		if inFlight {
			return ErrUncertain
		}
		if count >= service.options.MaxOperations {
			return ErrOperationLimit
		}

		operation, err = scanOperation(transaction.QueryRowContext(ctx, `INSERT INTO tombot.run_operations(user_id,id,run_id,step_key,kind,request_digest,fencing_token,attempt,status,computer_session_id)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,'started',$9) RETURNING `+operationColumns, lease.UserID, ids.New(), lease.RunID, request.StepKey, request.Kind, request.RequestDigest, lease.FencingToken, lease.Attempt, computerID))
		if err != nil {
			return err
		}
		if request.Model != nil {
			if err = modelusage.Reserve(ctx, transaction, usageReference(lease, operation), request.Model.Configuration, request.Model.Lifetime); err != nil {
				return err
			}
		}
		created = true
		return nil
	})
	if err != nil {
		return Operation{}, false, err
	}
	return operation, created, nil
}

// RecordOperation 在有效原租约下记录本次外部结果；结果未知显式保存，不使用默认成功。
// 重复相同终态幂等，冲突结果和旧执行者覆盖均拒绝；迟到证据需要独立对账入口。
func (service *Service) RecordOperation(ctx context.Context, lease Lease, id string, result OperationResult) (Operation, error) {
	if err := validateOperationScreenshot(lease, id, result); err != nil {
		return Operation{}, err
	}
	var object map[string]json.RawMessage
	if !validID(id) || (result.Status != "succeeded" && result.Status != "failed" && result.Status != "unknown") || len(result.Result) > 60<<10 || json.Unmarshal(result.Result, &object) != nil || object == nil ||
		(result.Status == "succeeded" && result.ErrorCode != "") || (result.Status != "succeeded" && !codePattern.MatchString(result.ErrorCode)) {
		return Operation{}, ErrInvalidInput
	}
	var operation Operation
	err := service.worker(ctx, lease, true, func(transaction *sql.Tx) error {
		var err error
		operation, err = scanOperation(transaction.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2 AND id=$3`, lease.UserID, lease.RunID, id))
		if err != nil {
			return err
		}
		if operation.Status != "started" {
			var same bool
			if err = transaction.QueryRowContext(ctx, `SELECT status=$4 AND result=$5::jsonb AND error_code=$6 FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2 AND id=$3`, lease.UserID, lease.RunID, id, result.Status, string(result.Result), result.ErrorCode).Scan(&same); err != nil {
				return err
			}
			if !same {
				return ErrConflict
			}
			return recordUsage(ctx, transaction, lease, operation, result)
		}
		if operation.FencingToken != lease.FencingToken || operation.Attempt != lease.Attempt {
			return ErrLeaseLost
		}
		if result.Screenshot != nil {
			if err := service.saveScreenshot(ctx, transaction, lease, operation, result.Screenshot); err != nil {
				return err
			}
		}
		operation, err = scanOperation(transaction.QueryRowContext(ctx, `UPDATE tombot.run_operations SET status=$4,result=$5,error_code=$6,finished_at=clock_timestamp()
			WHERE user_id=$1 AND run_id=$2 AND id=$3 RETURNING `+operationColumns, lease.UserID, lease.RunID, id, result.Status, string(result.Result), result.ErrorCode))
		if err != nil {
			return err
		}
		return recordUsage(ctx, transaction, lease, operation, result)
	})
	if err != nil {
		return Operation{}, err
	}
	return operation, nil
}

func usageReference(lease Lease, operation Operation) modelusage.Reference {
	return modelusage.Reference{UserID: lease.UserID, OperationID: operation.ID, RunID: lease.RunID, FencingToken: operation.FencingToken, Attempt: operation.Attempt}
}
func recordUsage(ctx context.Context, transaction *sql.Tx, lease Lease, operation Operation, result OperationResult) error {
	if operation.Kind != "model" {
		if result.Usage != nil || result.UsageFinal {
			return ErrInvalidInput
		}
		return nil
	}
	return modelusage.Settle(ctx, transaction, usageReference(lease, operation), operation.ID, modelusage.Observation{Usage: result.Usage, Final: result.UsageFinal})
}

// AppendModelEvidence 仅追加原操作的计量证据，不要求旧租约仍有效，也不修改运行终态。
// 调用者是受信 Worker 或对账器；普通 HTTP 接口不得暴露此动作。
func (service *Service) AppendModelEvidence(ctx context.Context, lease Lease, operationID, evidenceID string, observation modelusage.Observation) error {
	if !validLease(lease) || !validID(operationID) || !validID(evidenceID) {
		return ErrInvalidInput
	}
	return translate(service.tenant.Tenant(ctx, lease.UserID, func(transaction *sql.Tx) error {
		operation, err := scanOperation(transaction.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM tombot.run_operations WHERE user_id=$1 AND id=$2 AND run_id=$3 AND kind='model' AND fencing_token=$4 AND attempt=$5`, lease.UserID, operationID, lease.RunID, lease.FencingToken, lease.Attempt))
		if err != nil {
			return err
		}
		var same bool
		if err = transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.jobs WHERE user_id=$1 AND id=$2 AND run_id=$3 AND bot_id=$4 AND conversation_id=$5 AND turn_id=$6)`, lease.UserID, lease.JobID, lease.RunID, lease.BotID, lease.ConversationID, lease.TurnID).Scan(&same); err != nil {
			return err
		}
		if !same {
			return ErrLeaseLost
		}
		return modelusage.AppendEvidence(ctx, transaction, usageReference(lease, operation), evidenceID, observation)
	}))
}

// ReconcileUsage 处理真实调用硬时限与迟到用量；独立于运行租约恢复。
func (service *Service) ReconcileUsage(ctx context.Context, limit int) (int, error) {
	count, err := service.meter.Reconcile(ctx, limit)
	return count, translate(err)
}
