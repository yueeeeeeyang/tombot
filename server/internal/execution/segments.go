package execution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/provider"
)

type checkpointTool struct{}

func (checkpointTool) Schema() provider.ToolSchema {
	return provider.ToolSchema{Name: "continue_work", Description: "Deliver a verified partial result and schedule the remaining work. Use only when there is concrete progress and unfinished work.", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"result":{"type":"string"},"handoff":{"type":"string"}},"required":["result","handoff"]}`)}
}
func (checkpointTool) UsesComputer() bool   { return false }
func (checkpointTool) RequiresVision() bool { return false }

type workCheckpoint struct {
	Result  string `json:"result"`
	Handoff string `json:"handoff"`
	digest  [32]byte
}

func (*workCheckpoint) Error() string { return "阶段成果等待提交" }

var errNoProgress = errors.New("任务重复尝试，尚无新的可核实结果")

// 在单次租约、操作数或上下文预算耗尽前预留模型收尾机会；每个有实际成果的阶段可独立续跑。
func (relay *workerRelay) atSegmentBoundary(key string, operations, inputBytes int) bool {
	step, err := strconv.Atoi(strings.TrimPrefix(key, "model."))
	if err != nil || step <= 1 {
		return false
	}
	return step >= min(64, relay.worker.options.MaxModelSteps+1) || operations+2 >= relay.worker.executor.options.MaxOperations ||
		inputBytes > relay.worker.executor.options.ContextBytes*3/4 || time.Since(relay.segmentStarted) > relay.worker.options.RunTimeout*2/3
}

// 只使用真实成功工具回执构造阶段证据；不同调用编号、重复轮询及进度文案不能伪造进展。
func segmentEvidence(input modelgateway.Input) ([]byte, bool) {
	calls := map[string]string{}
	receipts := map[string]bool{}
	for _, m := range input.Messages {
		for _, block := range m.Blocks {
			if c := block.ToolUse; c != nil && c.Name != "report_progress" {
				arguments, err := json.Marshal(c.Input)
				if err != nil {
					return nil, false
				}
				calls[c.ID] = c.Name + "\x00" + string(arguments)
			}
			if r := block.ToolResult; r != nil && !r.IsError && calls[r.ToolCallID] != "" {
				fact := calls[r.ToolCallID] + "\x00" + r.Content
				if r.Artifact != nil {
					fact += "\x00" + r.Artifact.SHA256
				}
				receipts[fact] = true
			}
		}
	}
	facts := make([]string, 0, len(receipts))
	for fact := range receipts {
		facts = append(facts, fact)
	}
	sort.Strings(facts)
	encoded, err := json.Marshal(facts)
	return encoded, err == nil && len(facts) > 0
}

// 阶段控制响应留在模型操作账本，容器端只接收已确认的本段最终答复；恢复时沿用同一回执。
func (relay *workerRelay) segmentOutput(output botagent.ModelOutput, input modelgateway.Input) (botagent.ModelOutput, error) {
	for _, call := range output.Tools {
		if call.Name != "continue_work" {
			continue
		}
		var cp workCheckpoint
		if len(output.Tools) != 1 || decodeToolInput(call.Arguments, &cp, checkpointTool{}.Schema()) != nil || !validText(cp.Result, relay.worker.options.MaxOutputBytes) || !validText(cp.Handoff, 16<<10) {
			return botagent.ModelOutput{}, botagent.ErrModelOutput
		}
		evidence, progress := segmentEvidence(input)
		if !progress {
			return botagent.ModelOutput{}, errNoProgress
		}
		cp.digest = sha256.Sum256(evidence)
		relay.checkpoint = &cp
		return botagent.ModelOutput{Text: cp.Result, StopReason: "stop"}, nil
	}
	return output, nil
}

// advanceSegment 在租约锁下原子交付阶段结果、保存交接内容并重新排队。同一运行始终可取消。
func (s *Service) advanceSegment(ctx context.Context, l Lease, cp workCheckpoint) (Run, error) {
	var run Run
	err := s.worker(ctx, l, false, func(tx *sql.Tx) error {
		if err := available(ctx, tx, l); err != nil {
			return err
		}
		pending, err := computerPending(ctx, tx, l)
		if err != nil {
			return err
		}
		if pending {
			return ErrComputerBusy
		}
		lost, err := computerLost(ctx, tx, l)
		if err != nil {
			return err
		}
		if lost {
			return ErrComputerLost
		}

		var unknown bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.run_operations o WHERE o.user_id=r.user_id AND o.run_id=r.id AND o.segment=r.segment AND o.status IN ('started','unknown')) FROM tombot.runs r WHERE r.user_id=$1 AND r.id=$2`, l.UserID, l.RunID).Scan(&unknown); err != nil {
			return err
		}
		if unknown {
			return ErrUncertain
		}
		inserted, err := tx.ExecContext(ctx, `INSERT INTO tombot.run_checkpoints(user_id,run_id,segment,evidence_digest) SELECT user_id,id,segment,$3 FROM tombot.runs WHERE user_id=$1 AND id=$2 ON CONFLICT(user_id,run_id,evidence_digest) DO NOTHING`, l.UserID, l.RunID, cp.digest[:])
		if err != nil {
			return err
		}
		n, err := inserted.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errNoProgress
		}
		if err = writeGroupMessage(ctx, tx, l.UserID, l.ConversationID, l.TurnID, l.RunID, l.BotID, ids.New(), "", []Block{{Kind: "text", Text: cp.Result}}, "complete"); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tombot.runs SET segment=segment+1,handoff=$3,context_sequence=GREATEST(context_sequence,input_sequence),status='queued',progress_summary='准备继续下一阶段' WHERE user_id=$1 AND id=$2`, l.UserID, l.RunID, cp.Handoff); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tombot.jobs SET status='queued',attempt=0,fencing_token=fencing_token+1,lease_owner=NULL,lease_expires_at=NULL,available_at=clock_timestamp(),updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, l.UserID, l.JobID); err != nil {
			return err
		}
		if err = event(ctx, tx, l.UserID, "run.progress", l.RunID, map[string]string{"conversation_id": l.ConversationID, "bot_id": l.BotID, "text": "准备继续下一阶段"}); err != nil {
			return err
		}
		run, err = readRun(ctx, tx, l.UserID, l.RunID)
		return err
	})
	return run, err
}
