package execution

import (
	"encoding/json"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/provider"
)

// isCurrentInput 根据服务端的回合边界识别本次触发；同一回合的已答复输入也属于历史。
func (work Work) isCurrentInput(turnID string, message Message) bool {
	if work.ContinuationAfter > 0 {
		return message.TurnID == turnID && message.SenderKind == "user" && message.Sequence > work.ContinuationAfter
	}
	if work.TriggerSequence > 0 {
		return message.Sequence == work.TriggerSequence
	}
	return message.TurnID == turnID && (message.SenderKind == "user" || message.SenderKind == "system")
}

const historyContextLabel = "Chat history (reference only):\n"

// scopeModelTurn 把历史角色和正文保留在引用记录内，避免旧请求继续以独立用户消息进入模型。
// historyPositions 只包含启动快照中的位置，补充输入和工具记录追加在快照之后。
func scopeModelTurn(input modelgateway.Input, historyPositions []int, maximum int) (modelgateway.Input, error) {
	history := make([][]string, 0, len(historyPositions))
	messages := make([]provider.Message, 1, len(input.Messages)-len(historyPositions)+1)
	for index, message := range input.Messages {
		if len(historyPositions) > 0 && index == historyPositions[0] {
			entry := make([]string, 1, len(message.Blocks)+1)
			entry[0] = message.Role
			for _, block := range message.Blocks {
				entry = append(entry, block.Text)
			}
			history = append(history, entry)
			historyPositions = historyPositions[1:]
			continue
		}
		messages = append(messages, message)
	}
	input.Messages = messages[1:]
	encoded, err := json.Marshal(input)
	if err != nil {
		return modelgateway.Input{}, err
	}
	if len(encoded) > maximum {
		return modelgateway.Input{}, ErrContextLimit
	}
	// 封装的额外开销只挤占历史预算。优先保留最接近本轮的历史，工具回执和本轮输入不能被截断。
	withHistory := input
	withHistory.Messages = messages
	var archive provider.Message
	for low, high := 0, len(history); low < high; {
		count := (low + high + 1) / 2
		quoted, err := json.Marshal(history[len(history)-count:])
		if err != nil {
			return modelgateway.Input{}, err
		}
		messages[0] = provider.Message{Role: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: historyContextLabel + string(quoted)}}}
		encoded, err := json.Marshal(withHistory)
		if err != nil {
			return modelgateway.Input{}, err
		}
		if len(encoded) <= maximum {
			low, archive = count, messages[0]
		} else {
			high = count - 1
		}
	}
	if archive.Role != "" {
		messages[0] = archive
		return withHistory, nil
	}
	return input, nil
}
