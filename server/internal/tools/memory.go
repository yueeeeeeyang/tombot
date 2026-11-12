package tools

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/memory"
	"io.yueyang/tom/internal/policy"
)

type memoryInput struct {
	domain.MemoryInput
	Path           string `json:"path"`
	ExpectedSHA256 string `json:"expected_sha256"`
	Query          string `json:"query"`
	Limit          int    `json:"limit"`
}

func memoryDefinitions() map[string]definition {
	text := map[string]string{"type": "string"}
	integer := map[string]string{"type": "integer"}
	return map[string]definition{
		"memory_search": {"Search only this bot's private long-term memories. English uses stemming; CJK uses trigrams with a literal short-query fallback. Results include paths, summaries, snippets, SHA-256 and too_long. Use file_read on a returned path to expand it. Default limit: 5; maximum: 50.", map[string]any{"query": text, "limit": integer}, []string{"query"}, false},
		"memory_write":  {"Create an intentional private memory, never automatically copy a group or bot transcript. title and content are required; importance defaults to 3 (1 to 5), tags is a string array. summary, if supplied, becomes the first paragraph of the Markdown body. For updates supply path and the current expected_sha256 obtained from memory_search or file_read. Existing unknown front matter is preserved.", map[string]any{"title": text, "summary": text, "content": text, "tags": map[string]any{"type": "array", "items": text}, "importance": integer, "path": text, "expected_sha256": text}, []string{"title", "content"}, true},
		"memory_delete": {"Delete one of this bot's private memories. Requires a path directly under .tom/memory/. Optional expected_sha256 must match the current file and is rechecked immediately before deletion.", map[string]any{"path": text, "expected_sha256": text}, []string{"path"}, true},
	}
}

func (e *Executor) prepareMemory(ctx context.Context, bot domain.Bot, name string, raw []byte) (call preparedCall, err error) {
	var input memoryInput
	if err = json.Unmarshal(raw, &input); err != nil {
		return call, domain.NewError(400, "INVALID_TOOL_INPUT", "Invalid private memory arguments.")
	}
	action := policy.Action{Class: "fs.read", Pattern: memory.Directory, Risk: "low", Title: "检索私有记忆", Details: map[string]any{"bot_id": bot.ID}}
	if name == "memory_search" {
		if _, err = memory.ParseQuery(input.Query); err != nil {
			return call, err
		}
		if input.Limit < 0 || input.Limit > 50 {
			return call, domain.NewError(400, "INVALID_TOOL_INPUT", "The memory search limit must be between 1 and 50.")
		}
		return preparedCall{action: action, timeout: 30 * time.Second, input: input}, nil
	}
	if input.Path == "" && name == "memory_write" {
		input.Path = memory.Slug(input.Title)
	}
	if !memory.ValidPath(input.Path) {
		return call, domain.NewError(400, "MEMORY_PATH_INVALID", "Use a Markdown path directly under .tom/memory/.")
	}
	action.Pattern = input.Path
	action.Details["path"] = input.Path
	file, readErr := e.files.ReadMemory(ctx, bot.ID, input.Path)
	var previous *memory.Document
	if readErr != nil {
		var public *domain.Error
		if name != "memory_write" || input.ExpectedSHA256 != "" || !errors.As(readErr, &public) || public.Code != "FILE_NOT_FOUND" {
			return call, readErr
		}
	}
	binding := file.SHA256
	if input.ExpectedSHA256 != "" && input.ExpectedSHA256 != binding {
		return call, domain.NewError(409, "MEMORY_CHANGED", "The private memory changed after it was read.")
	}
	if name == "memory_delete" {
		input.ExpectedSHA256 = binding
		action.Class = "fs.delete"
		// 中风险：宽松策略的桌面猫咪可以在对话里直接忘掉，标准策略仍然要审批。
		action.Risk = "medium"
		action.Title = "删除私有记忆"
	} else {
		if readErr == nil {
			if input.ExpectedSHA256 == "" {
				return call, domain.NewError(409, "MEMORY_CHANGED", "Read the existing memory and supply expected_sha256 before updating it.")
			}
			doc, err := memory.Parse(input.Path, file.Content)
			if err != nil {
				return call, err
			}
			previous = &doc
		}
		var supplied map[string]json.RawMessage
		if err = json.Unmarshal(raw, &supplied); err != nil {
			return call, err
		}
		if _, exists := supplied["importance"]; !exists {
			input.Importance = 3
		}
		if _, err = memory.Render(previous, input.MemoryInput); err != nil {
			return call, err
		}
		action.Class = "fs.write_outside_out"
		action.Risk = "medium"
		action.Title = "保存私有记忆"
		action.Details["标题"] = input.Title
		action.Details["正文长度"] = len(input.Content)
	}
	return preparedCall{action: action, timeout: 30 * time.Second, input: input, binding: binding}, nil
}

func (e *Executor) invokeMemory(ctx context.Context, botID, name string, input memoryInput) (any, error) {
	switch name {
	case "memory_search":
		return e.memories.Search(ctx, botID, input.Query, input.Limit)
	case "memory_write":
		return e.memories.Write(ctx, botID, input.Path, input.MemoryInput, input.ExpectedSHA256, "bot:"+botID)
	case "memory_delete":
		if err := e.memories.Delete(ctx, botID, input.Path, input.ExpectedSHA256, "bot:"+botID); err != nil {
			return nil, err
		}
		return map[string]any{"deleted": true, "path": input.Path}, nil
	}
	return nil, domain.NewError(400, "TOOL_NOT_ALLOWED", "This memory tool is unavailable.")
}
