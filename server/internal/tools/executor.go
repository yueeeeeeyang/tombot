package tools

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	safelog "io.yueyang/tom/internal/infra/log"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/secret"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/store"
	"io.yueyang/tom/internal/term"
	"io.yueyang/tom/internal/usage"
	"io.yueyang/tom/internal/workspace"
)

type Executor struct {
	db            *sql.DB
	events        *bus.Bus
	files         *workspace.Files
	memories      *service.Memories
	routines      *service.Routines
	conversations *service.Conversations
	approvals     *service.Approvals
	secrets       *secret.Manager
	shell         *term.Shell
	terminals     *service.Terminals
	browser       *service.Browser
	reviewer      *policy.Reviewer
	definitions   map[string]definition
	schemas       map[string]json.RawMessage
}

func New(db *sql.DB, events *bus.Bus, files *workspace.Files, approvals *service.Approvals, secrets *secret.Manager, terminals *service.Terminals, browser *service.Browser, reviewer *policy.Reviewer, memories *service.Memories) (*Executor, error) {
	if terminals == nil || browser == nil || reviewer == nil || memories == nil {
		return nil, errors.New("工具执行器缺少终端、浏览器、审查或记忆服务")
	}
	e := &Executor{db: db, events: events, files: files, memories: memories, routines: service.NewRoutines(db, events, nil), approvals: approvals, secrets: secrets, terminals: terminals, browser: browser, reviewer: reviewer, shell: term.NewShell(), definitions: definitions(), schemas: map[string]json.RawMessage{}}
	e.conversations = service.NewConversations(db, events)
	for name, definition := range e.definitions {
		encoded, err := json.Marshal(map[string]any{"type": "object", "properties": definition.properties, "required": definition.required, "additionalProperties": false})
		if err != nil {
			return nil, err
		}
		e.schemas[name] = encoded
	}
	return e, nil
}

// Execute 只把预期工具失败回灌给模型；持久化和基础设施失败向上传播，避免无记录的动作。
func (e *Executor) Execute(ctx context.Context, invocation domain.ToolInvocation, use domain.ToolUseBlock) (result domain.ToolResultBlock, err error) {
	run, bot, step := invocation.Run, invocation.Bot, invocation.Step
	result.ToolCallID = use.ID
	started := time.Now()
	definition, known := e.definitions[use.Name]
	allowed := false
	for _, name := range bot.ToolPolicy.AllowedTools {
		if name == use.Name {
			allowed = true
			break
		}
	}
	raw, err := json.Marshal(use.Input)
	if err != nil {
		return result, err
	}
	input, inputErr := e.prepare(ctx, bot, use.Name, raw, definition)
	if !known || !allowed {
		inputErr = domain.NewError(403, "TOOL_NOT_ALLOWED", "This tool is not allowed for the bot.")
	}
	action := input.action
	// 不把文件正文、搜索表达式或替换文本写入审计/调用摘要，具体内容仍保留在消息记录。
	preview := map[string]any{"tool": use.Name, "argument_bytes": len(raw)}
	if inputErr == nil {
		preview["path"] = safelog.SafeText(action.Pattern)
	}
	encoded, err := json.Marshal(preview)
	if err != nil {
		return result, err
	}
	err = store.Transact(ctx, e.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tool_calls(id,run_id,step,tool_name,input_json,status,created_at) VALUES(?,?,?,?,?,'pending',?)`, use.ID, run.ID, step, use.Name, string(encoded), started.UnixMilli()); err != nil {
			return err
		}
		return service.AppendToolActivity(ctx, tx, run.ConversationID, use.ID, "tool.started")
	})
	if err != nil {
		return result, err
	}
	e.events.Notify()
	status, code := "succeeded", ""
	defer func() {
		if err != nil || result.IsError {
			status = "failed"
		}
		if err != nil {
			code = "TOOL_FAILED"
			var public *domain.Error
			if errors.As(err, &public) {
				code = public.Code
			}
		}
		if code == "TOOL_DENIED" || code == "TOOL_NOT_ALLOWED" {
			status = "denied"
		}
		if errors.Is(err, context.Canceled) {
			status = "canceled"
			code = "TOOL_CANCELED"
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		output, encodeErr := json.Marshal(result)
		if encodeErr != nil {
			err = errors.Join(err, encodeErr)
			return
		}
		persistErr := store.Transact(cleanup, e.db, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(cleanup, "UPDATE tool_calls SET status=?,error_code=NULLIF(?,''),output_json=?,duration_ms=?,finished_at=? WHERE id=?", status, code, string(output), time.Since(started).Milliseconds(), time.Now().UnixMilli(), use.ID); err != nil {
				return err
			}
			return service.AppendToolActivity(cleanup, tx, run.ConversationID, use.ID, "tool.output")
		})
		err = errors.Join(err, persistErr)
		if persistErr == nil {
			e.events.Notify()
		}
	}()
	fail := func(cause error) (domain.ToolResultBlock, error) {
		var public *domain.Error
		if errors.As(cause, &public) {
			if public.Code == "BROWSER_STATE_FAILED" {
				return result, cause
			}
			code = public.Code
			result.IsError = true
			result.Content = public.Error()
			return result, nil
		}
		return result, cause
	}
	if inputErr != nil {
		return fail(inputErr)
	}
	decision, err := e.approvals.PolicyDecision(ctx, bot.ID, action)
	if err != nil {
		return result, err
	}
	if decision == "deny" || decision == "" && action.Class == "shell.privileged" {
		return fail(domain.NewError(403, "TOOL_DENIED", "A policy rule denied this action."))
	}
	if err = usage.CheckBudget(ctx, e.db, run.ID); err != nil {
		return result, err
	}
	if use.Name == "ask_user" {
		return fail(domain.NewError(410, "USER_QUESTION_REMOVED", "Asking the user through an approval is no longer available."))
	}
	if _, err = policy.NeedsApproval(bot.ToolPolicy.Approval, action.Risk); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = usage.CheckBudget(ctx, e.db, run.ID); err != nil {
		return result, err
	}
	// 审批期间路径可能变化；重新提升的风险必须重新发起调用，不能复用较低风险许可。
	current, err := e.prepare(ctx, bot, use.Name, raw, definition)
	if err != nil {
		return fail(err)
	}
	if current.action.Class != action.Class || current.action.Pattern != action.Pattern || current.binding != input.binding || current.action.ForceApproval != action.ForceApproval || policy.IsHigherRisk(current.action.Risk, input.action.Risk) {
		return fail(domain.NewError(409, "TOOL_TARGET_CHANGED", "The target changed while waiting. Submit a new tool call."))
	}
	currentDecision, err := e.approvals.PolicyDecision(ctx, bot.ID, current.action)
	if err != nil {
		return result, err
	}
	if currentDecision == "deny" {
		return fail(domain.NewError(403, "TOOL_DENIED", "A policy rule now denies this action."))
	}
	if decision == "allow" && currentDecision != "allow" {
		return fail(domain.NewError(409, "TOOL_TARGET_CHANGED", "The policy changed while waiting. Submit a new tool call."))
	}
	if err = store.Transact(ctx, e.db, func(tx *sql.Tx) error {
		if err := store.RequireChanged(tx.ExecContext(ctx, "UPDATE tool_calls SET status='running' WHERE id=? AND status='pending'", use.ID)); err != nil {
			return err
		}
		return service.AppendToolActivity(ctx, tx, run.ConversationID, use.ID, "tool.status")
	}); err != nil {
		return result, err
	}
	e.events.Notify()
	timeout, cancel := context.WithTimeout(ctx, input.timeout)
	defer cancel()
	audit := func(ctx context.Context, path, hash string) error {
		return store.Transact(ctx, e.db, func(tx *sql.Tx) error {
			return store.Audit(ctx, tx, "bot:"+bot.ID, "fs.write", use.ID, map[string]string{"run_id": run.ID, "bot_id": run.BotID, "path": path, "old_sha256": hash})
		})
	}
	var value any
	if use.Name == "computer_shell" {
		value, err = e.invokeShell(timeout, run, step, use.ID, input.input.(term.ShellInput), audit)
	} else if use.Name == "terminal_session" {
		value, err = e.invokeTerminal(timeout, run, input.input.(terminalInput))
	} else if use.Name == "browser" {
		value, err = e.invokeBrowser(timeout, run, use.ID, input.input.(browserInput), audit)
	} else if use.Name == "group_members" || use.Name == "post_to_group" {
		value, err = e.invokeGroup(timeout, run, use.ID, use.Name, input.input.(groupInput))
	} else if use.Name == "message_bot" || use.Name == "delegate_task" {
		value, err = e.invokeMessaging(timeout, invocation, use.ID, input.input.(domain.BotMessageRequest))
	} else if use.Name == "message_user" {
		value, err = invokeUserMessage(timeout, invocation, use.ID, input.input.(string))
	} else if use.Name == "memory_search" || use.Name == "memory_write" || use.Name == "memory_delete" {
		value, err = e.invokeMemory(timeout, bot.ID, use.Name, input.input.(memoryInput))
	} else if use.Name == "routine_list" || use.Name == "routine_upsert" || use.Name == "routine_delete" {
		value, err = e.invokeRoutine(timeout, bot.ID, use.Name, input.input.(routineInput))
	} else if use.Name == "conversation_search" {
		value, err = e.conversations.SearchTranscript(timeout, bot.ID, input.input.(domain.ConversationSearch))
	} else if use.Name == "conversation_read" {
		value, err = e.conversations.ReadTranscript(timeout, bot.ID, input.input.(domain.ConversationRead))
	} else {
		value, err = e.invokeFile(timeout, bot.ID, use.Name, input.input.(fileInput), audit)
	}
	if artifact, ok := value.(*domain.ArtifactRef); ok && err == nil {
		result.Artifact = artifact
	}
	if value != nil {
		encoded, encodeErr := json.Marshal(value)
		if encodeErr != nil {
			return result, encodeErr
		}
		result.Content = string(encoded)
		if failed, ok := value.(interface{ FailureCode() string }); ok && failed.FailureCode() != "" {
			code = failed.FailureCode()
			result.IsError = true
		}
	}
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		if use.Name == "computer_shell" && value != nil {
			code = "TOOL_TIMEOUT"
			result.IsError = true
			return result, nil
		}
		return fail(domain.NewError(408, "TOOL_TIMEOUT", "The tool reached its time limit."))
	}
	if err != nil {
		return fail(err)
	}
	output, err := json.Marshal(value)
	if err != nil {
		return result, err
	}
	if len(output) > 32<<10 && use.Name != "computer_shell" {
		// 按行展开结构，后续读取可选取具体条目，而非再次读入单行巨型 JSON。
		var formatted bytes.Buffer
		if err = json.Indent(&formatted, output, "", "  "); err != nil {
			return result, err
		}
		file, saveErr := e.files.SaveOutput(timeout, bot.ID, use.ID, formatted.Bytes(), audit)
		if saveErr != nil {
			return result, saveErr
		}
		// 确定性摘要保留首尾，完整 JSON 供后续文件窗口读取。
		characters := []rune(string(output))
		preview := string(characters[:min(4000, len(characters))]) + "\n[... output truncated ...]\n" + string(characters[max(0, len(characters)-4000):])
		output, err = json.Marshal(map[string]any{"summary": preview, "truncated": true, "output_path": file.Path, "bytes": len(output)})
		if err != nil {
			return result, err
		}
	}
	result.Content = string(output)
	if definition.write {
		if _, err = e.events.Publish(ctx, "artifact.created", run.ConversationID, map[string]string{"run_id": run.ID, "bot_id": run.BotID, "path": action.Pattern, "kind": "file"}); err != nil {
			return result, fmt.Errorf("产物事件保存失败: %w", err)
		}
	}
	return result, nil
}
