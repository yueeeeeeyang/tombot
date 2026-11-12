package execution

import (
	"context"

	"io.yueyang/tom/internal/botagent"
)

type localOperation struct {
	operation Operation
	grant     botagent.ToolGrant
}

// PrepareLocal 只登记原模型提出的工具调用；返回授权或已有回执，绝不调用节点执行浏览器动作。
func (relay *workerRelay) PrepareLocal(ctx context.Context, key string, call botagent.ToolCall) (botagent.ToolPreparation, error) {
	tool := relay.allowed[call.Name]
	if relay.pendingLocal != nil || !runsInAgent(tool) {
		return botagent.ToolPreparation{}, botagent.ErrProtocol
	}
	if err := relay.worker.computers.Check(ctx, computerReference(relay.lease)); err != nil {
		return botagent.ToolPreparation{}, err
	}
	operation, cached, err := relay.worker.prepareTool(ctx, relay.lease, relay.session, key, call, tool, relay.previous)
	if err != nil {
		return botagent.ToolPreparation{}, err
	}
	if cached != nil {
		return botagent.ToolPreparation{Result: cached}, nil
	}
	grant := botagent.ToolGrant{OperationID: operation.ID, Sequence: operation.Sequence, Session: relay.session, ApprovalMode: relay.approvalMode, TimeoutMS: relay.worker.options.ToolTimeout.Milliseconds(), OutputBytes: relay.worker.options.MaxOutputBytes}
	if grant.Validate() != nil {
		_, err := relay.worker.recordTool(ctx, relay.lease, operation, botagent.ToolResult{}, "TOOL_RESULT_INVALID")
		return botagent.ToolPreparation{}, err
	}
	relay.pendingLocal = &localOperation{operation: operation, grant: grant}
	return botagent.ToolPreparation{Grant: &grant}, nil
}

// CompleteLocal 只接受当前待确认的操作；断连仍用独立有界期限保存未知结果，不触发重放。
func (relay *workerRelay) CompleteLocal(ctx context.Context, grant botagent.ToolGrant, report botagent.ToolReport) (botagent.ToolResult, error) {
	pending := relay.pendingLocal
	if pending == nil || pending.grant != grant {
		return botagent.ToolResult{}, botagent.ErrProtocol
	}
	relay.pendingLocal = nil
	bounded, cancel := context.WithTimeout(ctx, relay.worker.options.RecordTimeout)
	defer cancel()
	if report.Validate(grant) != nil {
		report.Failure = "TOOL_RESULT_INVALID"
	} else if report.Failure == "" {
		if err := relay.worker.computers.Check(bounded, computerReference(relay.lease)); err != nil {
			report.Failure = "TOOL_RESULT_UNKNOWN"
		}
	}
	return relay.worker.recordTool(bounded, relay.lease, pending.operation, report.Result, report.Failure)
}

var _ botagent.LocalRelay = (*workerRelay)(nil)
