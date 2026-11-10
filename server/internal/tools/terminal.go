package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/term"
)

type terminalInput struct {
	Action   string `json:"action"`
	ID       string `json:"id,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
	Title    string `json:"title,omitempty"`
	Data     string `json:"data,omitempty"`
	Columns  int    `json:"cols,omitempty"`
	Rows     int    `json:"rows,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Revision int64  `json:"revision,omitempty"`
}

func terminalDefinition() definition {
	text := map[string]any{"type": "string"}
	return definition{
		description: "Manage persistent interactive PTYs with list/open/write/read/resize/close. List returns your latest 50 terminal records, including user-created terminals assigned to you. Open accepts cwd/title/cols/rows; remaining actions require id. Write accepts data (carriage return submits); read accepts limit (4-16384 bytes); resize requires cols and rows. Reads consume your incremental cursor; users have separate cursors and may take control. Writes require approval and must never contain secrets. Never retry failed input: a prefix may have been delivered. Sessions remain 30 minutes after the last run ends; server restart loses processes and output. Do not start detached daemons or privileged operations.",
		properties: map[string]any{
			"action": map[string]any{"type": "string", "enum": []string{"list", "open", "write", "read", "resize", "close"}},
			"id":     text, "cwd": text, "title": text, "data": text,
			"cols":  map[string]any{"type": "integer", "minimum": 2, "maximum": 500},
			"rows":  map[string]any{"type": "integer", "minimum": 2, "maximum": 300},
			"limit": map[string]any{"type": "integer", "minimum": 4, "maximum": 16384},
		}, required: []string{"action"},
	}
}

func (e *Executor) prepareTerminal(ctx context.Context, bot domain.Bot, raw []byte) (preparedCall, error) {
	var input terminalInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return preparedCall{}, domain.NewError(400, "INVALID_TOOL_INPUT", "A terminal argument has the wrong type.")
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return preparedCall{}, err
	}
	allowed := map[string]bool{"action": true, "id": true}
	class, risk, title := "shell.exec", "medium", "操作终端会话"
	details := map[string]any{"tool": "terminal_session", "action": input.Action, "terminal_id": input.ID}
	switch input.Action {
	case "list":
		allowed = map[string]bool{"action": true}
		risk, title = "low", "查看终端会话"
	case "open":
		allowed = map[string]bool{"action": true, "cwd": true, "title": true, "cols": true, "rows": true}
		if input.Columns == 0 {
			input.Columns = 100
		}
		if input.Rows == 0 {
			input.Rows = 30
		}
		if err := term.ValidateSize(input.Columns, input.Rows); err != nil {
			return preparedCall{}, err
		}
		cwd, err := e.files.Directory(bot.ID, input.Cwd)
		if err != nil {
			return preparedCall{}, err
		}
		input.Cwd, _, err = e.files.Describe(bot.ID, cwd)
		if err != nil {
			return preparedCall{}, err
		}
		title, details["cwd"] = "创建持久终端", input.Cwd
	case "write":
		allowed["data"] = true
		if err := term.ValidateSessionInput(input.Data); err != nil {
			return preparedCall{}, err
		}
		class, risk, title = policy.ShellClass(input.Data), "high", "向终端输入内容"
		details["command"] = input.Data
	case "read":
		allowed["limit"] = true
		if input.Limit == 0 {
			input.Limit = 4096
		}
		if input.Limit < 4 || input.Limit > 16384 {
			return preparedCall{}, domain.NewError(400, "INVALID_TOOL_INPUT", "Terminal read limit must be between 4 and 16384 bytes.")
		}
		risk, title = "low", "读取终端输出"
	case "resize":
		allowed["cols"], allowed["rows"] = true, true
		if err := term.ValidateSize(input.Columns, input.Rows); err != nil {
			return preparedCall{}, err
		}
		risk, title = "low", "调整终端尺寸"
	case "close":
		title = "关闭终端会话"
	default:
		return preparedCall{}, domain.NewError(400, "INVALID_TOOL_INPUT", "Terminal action must be list, open, write, read, resize or close.")
	}
	for field, value := range fields {
		if !allowed[field] || (field == "cols" || field == "rows" || field == "limit") && string(value) == "0" {
			return preparedCall{}, domain.NewError(400, "INVALID_TOOL_INPUT", "The argument is not valid for this terminal action.")
		}
	}
	if input.Action != "open" && input.Action != "list" {
		if input.ID == "" {
			return preparedCall{}, domain.NewError(400, "INVALID_TOOL_INPUT", "This terminal action requires an id.")
		}
		terminal, err := e.terminals.Get(ctx, input.ID)
		if err != nil {
			return preparedCall{}, err
		}
		if terminal.BotID != bot.ID {
			return preparedCall{}, domain.NewError(403, "TERMINAL_FORBIDDEN", "A bot may only use terminals assigned to that bot.")
		}
		if input.Action != "read" {
			if terminal.Controller != "bot" {
				return preparedCall{}, domain.NewError(409, "TERMINAL_CONTROL_CHANGED", "The user controls this terminal. Wait until control is returned.")
			}
			input.Revision = terminal.Revision
		}
		details["terminal_name"] = terminal.Name
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return preparedCall{}, err
	}
	digest := sha256.Sum256(encoded)
	return preparedCall{input: input, timeout: 30 * time.Second, action: policy.Action{Class: class, Risk: risk, Pattern: "sha256:" + hex.EncodeToString(digest[:]), Title: title, Details: details}}, nil
}

func (e *Executor) invokeTerminal(ctx context.Context, run domain.Run, input terminalInput) (any, error) {
	actor := "bot:" + run.BotID
	switch input.Action {
	case "list":
		return e.terminals.List(ctx, run.BotID, "", 50)
	case "open":
		return e.terminals.Open(ctx, service.TerminalOpenInput{Cwd: input.Cwd, Title: input.Title, Columns: input.Columns, Rows: input.Rows}, &run, actor)
	case "read":
		return e.terminals.ReadBot(ctx, input.ID, run, input.Limit)
	case "write":
		if err := e.terminals.Write(ctx, input.ID, input.Data, &run, input.Revision, actor); err != nil {
			return nil, err
		}
	case "resize":
		if err := e.terminals.Resize(ctx, input.ID, input.Columns, input.Rows, &run, input.Revision, actor); err != nil {
			return nil, err
		}
	case "close":
		if err := e.terminals.Close(ctx, input.ID, &run, input.Revision, actor); err != nil {
			return nil, err
		}
	}
	return e.terminals.Get(ctx, input.ID)
}
