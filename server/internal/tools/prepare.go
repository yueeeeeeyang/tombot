package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/term"
)

type preparedCall struct {
	action  policy.Action
	timeout time.Duration
	input   any
	binding string
}

// prepare 仅校验和归类，不产生外部副作用；审批后重复执行以发现目标变化。
func (e *Executor) prepare(ctx context.Context, bot domain.Bot, name string, raw []byte, definition definition) (call preparedCall, err error) {
	if err = validateArguments(raw, definition); err != nil {
		return call, err
	}
	if name == "ask_user" {
		return prepareQuestion(raw)
	}
	if name == "terminal_session" {
		return e.prepareTerminal(ctx, bot, raw)
	}
	if name == "conversation_search" || name == "conversation_read" {
		return e.prepareConversation(ctx, bot, name, raw)
	}
	if name == "browser" {
		return e.prepareBrowser(ctx, bot, raw)
	}
	if name == "group_members" || name == "post_to_group" {
		return e.prepareGroup(ctx, bot, name, raw)
	}
	if name == "message_bot" || name == "delegate_task" {
		return e.prepareMessaging(ctx, bot, name, raw)
	}
	if name == "memory_search" || name == "memory_write" || name == "memory_delete" {
		return e.prepareMemory(ctx, bot, name, raw)
	}
	if name == "computer_shell" {
		var input term.ShellInput
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&input); err != nil {
			return call, domain.NewError(400, "INVALID_TOOL_INPUT", "A shell argument is invalid.")
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(raw, &fields); err != nil {
			return call, err
		}
		if _, supplied := fields["timeout_seconds"]; supplied && input.TimeoutSeconds == 0 {
			return call, domain.NewError(400, "INVALID_TOOL_INPUT", "An explicit shell timeout must be between 1 and 600 seconds.")
		}
		if err = term.ValidateShell(input); err != nil {
			return call, err
		}
		if input.Cwd == "" {
			input.Cwd = "."
		}
		call.action, err = e.shellAction(bot, input)
		call.input = input
		call.timeout = term.ShellTimeout(input)
		return call, err
	}
	var input fileInput
	if err = json.Unmarshal(raw, &input); err != nil {
		return call, domain.NewError(400, "INVALID_TOOL_INPUT", "A file argument has the wrong type.")
	}
	if input.Path == "" {
		input.Path = "."
	}
	call.action, err = e.fileAction(bot, name, input, definition.write)
	call.input = input
	call.timeout = 30 * time.Second
	return call, err
}
