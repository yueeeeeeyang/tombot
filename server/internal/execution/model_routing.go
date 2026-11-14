package execution

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
)

var errModelFailover = errors.New("模型调用未产生输出，可尝试备用候选")

const modelFailoverBackoff = 200 * time.Millisecond

// modelRequirements 使用消息实际能力和授权工具筛选；文本按 UTF-8 字节保守预留，图片沿用尺寸预算。
// 这是路由容量估算，供应商仍会按自身分词器校验上下文；不能把估算当实际计费用量。
func modelRequirements(input modelgateway.Input, allowed map[string]Tool) (modelregistry.Requirements, error) {
	requirements := modelregistry.Requirements{Tools: len(input.Tools) > 0, InputTokens: len(input.System)}
	for _, tool := range allowed {
		requirements.Vision = requirements.Vision || tool.RequiresVision()
	}
	addImage := func(ref *domain.ArtifactRef) {
		if ref == nil || ref.Unavailable {
			return
		}
		if ref.IsImage() {
			requirements.Vision = true
			requirements.InputTokens += ref.VisionTokens()
		} else {
			requirements.InputTokens += len(ref.Content)
		}
	}
	for _, message := range input.Messages {
		requirements.InputTokens += len(message.Role)
		for _, block := range message.Blocks {
			requirements.InputTokens += len(block.Text)
			addImage(block.Artifact)
			if block.ToolUse != nil {
				encoded, err := json.Marshal(block.ToolUse)
				if err != nil {
					return requirements, err
				}
				requirements.InputTokens += len(encoded)
			}
			if block.ToolResult != nil {
				requirements.InputTokens += len(block.ToolResult.Content) + len(block.ToolResult.ToolCallID)
				addImage(block.ToolResult.Artifact)
			}
		}
	}
	for _, tool := range input.Tools {
		encoded, err := json.Marshal(tool)
		if err != nil {
			return requirements, err
		}
		requirements.InputTokens += len(encoded)
	}
	return requirements, nil
}

// routeModel 由控制连接持久选择候选；日志仅记录业务标识，不包含上游内容或凭据。
func (relay *workerRelay) routeModel(ctx context.Context, input modelgateway.Input, failed string) error {
	requirements, err := modelRequirements(input, relay.allowed)
	if err != nil {
		return err
	}
	previous := relay.selection.ModelID
	selected, err := relay.worker.directory.Route(ctx, relay.selection, requirements, failed)
	if err != nil {
		return err
	}
	relay.selection = selected
	if previous != selected.ModelID {
		relay.worker.logger.InfoContext(ctx, "已选择运行模型", "user_id", relay.lease.UserID, "run_id", relay.lease.RunID, "policy_id", selected.PolicyID, "version", selected.Version, "model_id", selected.ModelID, "previous_model_id", previous, "is_failover", failed != "")
	}
	return nil
}
