package tools

import (
	"context"
	"encoding/json"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/service"
	"strings"
	"time"
)

func prepareUserMessage(raw []byte) (preparedCall, error) {
	var input struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return preparedCall{}, err
	}
	input.Message = strings.TrimSpace(input.Message)
	if err := service.ValidateUserMessage(input.Message); err != nil {
		return preparedCall{}, err
	}
	return preparedCall{action: policy.Action{Class: "message.user", Pattern: "user", Risk: "low", Title: "向用户发送消息", Details: map[string]any{"消息": input.Message}}, timeout: 10 * time.Second, input: input.Message}, nil
}

func invokeUserMessage(ctx context.Context, invocation domain.ToolInvocation, callID, message string) (domain.BotMessageReply, error) {
	messenger, ok := invocation.Messenger.(domain.UserMessenger)
	if !ok {
		return domain.BotMessageReply{}, domain.NewError(503, "BOT_MESSAGE_UNAVAILABLE", "This execution cannot deliver messages to the user.")
	}
	return messenger.SendUserMessage(ctx, invocation.Run, callID, message)
}
