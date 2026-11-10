package tools

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/store"
)

func messagingDefinitions() map[string]definition {
	text := map[string]string{"type": "string"}
	paths := map[string]any{"type": "array", "items": text, "maxItems": domain.MaxMessageContextFiles}
	return map[string]definition{
		"message_bot":   {"Send explicit text to another bot. The sender must have can_initiate permission and the recipient must accept direct messages. Defaults to waiting for a reply; reply_required=false returns after delivery while the child task remains bounded by the parent's time and budget. Up to four hops; no cycles. context_paths may contain up to eight files under shared/ (combined text at most 32768 bytes). Private prompts, memory and tool history are never forwarded.", map[string]any{"bot_id": text, "message": text, "reply_required": map[string]string{"type": "boolean"}, "context_paths": paths}, []string{"bot_id", "message"}, false},
		"delegate_task": {"Delegate one deliverable to another bot in a new child conversation. Waits for a structured result containing summary and verified shared artifact paths. Same messaging permissions, four-hop limit, inherited budget and deadline as message_bot. Put deliverables in shared/. context_paths may contain up to eight shared files with at most 32768 combined UTF-8 bytes.", map[string]any{"bot_id": text, "task": text, "context_paths": paths}, []string{"bot_id", "task"}, false},
	}
}

func (e *Executor) prepareMessaging(ctx context.Context, bot domain.Bot, name string, raw []byte) (call preparedCall, err error) {
	var input struct {
		BotID         string   `json:"bot_id"`
		Message       string   `json:"message"`
		Task          string   `json:"task"`
		ReplyRequired *bool    `json:"reply_required"`
		ContextPaths  []string `json:"context_paths"`
	}
	if err = json.Unmarshal(raw, &input); err != nil {
		return call, domain.NewError(400, "INVALID_TOOL_INPUT", "Invalid messaging arguments.")
	}
	request := domain.BotMessageRequest{BotID: input.BotID, Message: strings.TrimSpace(input.Message), ReplyRequired: true, Delegation: name == "delegate_task", ContextFiles: []domain.ContextFile{}}
	if input.ReplyRequired != nil {
		request.ReplyRequired = *input.ReplyRequired
	}
	if request.Delegation {
		request.Message = strings.TrimSpace(input.Task)
		request.ReplyRequired = true
	}
	if len(input.ContextPaths) > domain.MaxMessageContextFiles {
		return call, domain.NewError(400, "INVALID_TOOL_INPUT", "Too many context files.")
	}
	if err = service.ValidateBotMessage(request); err != nil {
		return call, err
	}
	var target domain.Bot
	err = store.Transact(ctx, e.db, func(tx *sql.Tx) error {
		var err error
		sender, err := service.ReadBot(ctx, tx, bot.ID)
		if err != nil {
			return err
		}
		target, err = service.ReadBot(ctx, tx, input.BotID)
		if err != nil {
			return err
		}
		return service.ValidateMessaging(sender, target)
	})
	if err != nil {
		return call, err
	}
	request.TargetBinding, err = service.MessagingBinding(target)
	if err != nil {
		return call, err
	}
	for _, path := range input.ContextPaths {
		if !service.SharedContextPath(path) {
			return call, domain.NewError(400, "INVALID_TOOL_INPUT", "Context files must be canonical paths under shared/.")
		}
		file, err := e.files.Text(ctx, bot.ID, path)
		if err != nil {
			return call, err
		}
		request.ContextFiles = append(request.ContextFiles, domain.ContextFile{Path: path, Content: file.Content, SHA256: file.SHA256})
	}
	if err = service.ValidateBotMessage(request); err != nil {
		return call, err
	}
	encoded, err := json.Marshal(struct {
		Target string
		Files  []domain.ContextFile
	}{request.TargetBinding, request.ContextFiles})
	if err != nil {
		return call, err
	}
	digest := sha256.Sum256(encoded)
	action := policy.Action{Class: "message.bot", Pattern: target.ID, Risk: "medium", Title: "向 Bot 发送私信", Details: map[string]any{"目标": target.Name, "消息": request.Message, "共享文件": input.ContextPaths, "等待回复": request.ReplyRequired}}
	if request.Delegation {
		action.Class = "delegate.spawn"
		action.Title = "向 Bot 委派任务"
	}
	return preparedCall{action: action, timeout: 24 * time.Hour, input: request, binding: hex.EncodeToString(digest[:])}, nil
}

func (e *Executor) invokeMessaging(ctx context.Context, invocation domain.ToolInvocation, callID string, input domain.BotMessageRequest) (any, error) {
	if invocation.Messenger == nil {
		return nil, domain.NewError(503, "BOT_MESSAGE_UNAVAILABLE", "This execution has no messaging runtime.")
	}
	reply, err := invocation.Messenger.SendBotMessage(ctx, invocation.Run, callID, input)
	if err != nil {
		return nil, err
	}
	for _, path := range reply.Artifacts {
		if !service.SharedContextPath(path) {
			return nil, domain.NewError(502, "BOT_ARTIFACT_INVALID", "The target returned a non-shared artifact path.")
		}
		file, err := e.files.Download(ctx, invocation.Bot.ID, path)
		if err != nil {
			return nil, err
		}
		if err = file.File.Close(); err != nil {
			return nil, err
		}
	}
	return reply, nil
}
