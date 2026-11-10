package tools

import (
	"context"
	"encoding/json"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/workspace"
)

type fileInput struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	OldText    string `json:"old_text"`
	NewText    string `json:"new_text"`
	Pattern    string `json:"pattern"`
	CreateDirs bool   `json:"create_dirs"`
	StartLine  int    `json:"start_line"`
	Limit      int    `json:"limit"`
	Depth      int    `json:"depth"`
}
type definition struct {
	description string
	properties  map[string]any
	required    []string
	write       bool
}

func definitions() map[string]definition {
	text := map[string]string{"type": "string"}
	integer := map[string]string{"type": "integer"}
	boolean := map[string]string{"type": "boolean"}
	registered := map[string]definition{
		"file_read":   {"Read a UTF-8 text file with line numbers. Default limit: 2000 lines; maximum file size: one MiB (16 MiB for saved tool output files).", map[string]any{"path": text, "start_line": integer, "limit": integer}, []string{"path"}, false},
		"file_write":  {"Replace a UTF-8 text file atomically. Set create_dirs to create missing parents. Only your workspace and shared/ are accessible.", map[string]any{"path": text, "content": text, "create_dirs": boolean}, []string{"path", "content"}, true},
		"file_edit":   {"Replace exactly one occurrence of old_text in a UTF-8 file. Missing or ambiguous matches fail without changes.", map[string]any{"path": text, "old_text": text, "new_text": text}, []string{"path", "old_text", "new_text"}, true},
		"file_list":   {"List workspace files by modification time, ignoring dependency directories. Default depth: 2; maximum: 16.", map[string]any{"path": text, "depth": integer}, []string{}, false},
		"file_glob":   {"Recursively match file names using filepath.Match syntax (*, ?, character classes), sorted by modification time.", map[string]any{"path": text, "pattern": text}, []string{"pattern"}, false},
		"file_search": {"Search UTF-8 files using a Go regular expression, without external commands. Default match limit: 100.", map[string]any{"path": text, "pattern": text, "limit": integer}, []string{"pattern"}, false},
	}
	registered["computer_shell"] = shellDefinition()
	registered["ask_user"] = questionDefinition()
	registered["terminal_session"] = terminalDefinition()
	registered["browser"] = browserDefinition()
	for name, definition := range groupDefinitions() {
		registered[name] = definition
	}
	for name, definition := range messagingDefinitions() {
		registered[name] = definition
	}
	for name, definition := range memoryDefinitions() {
		registered[name] = definition
	}
	for name, definition := range conversationDefinitions() {
		registered[name] = definition
	}
	return registered
}

func (e *Executor) Schemas(bot domain.Bot) []provider.ToolSchema {
	schemas := []provider.ToolSchema{}
	for _, name := range bot.ToolPolicy.AllowedTools {
		definition, ok := e.definitions[name]
		if !ok {
			continue
		}
		// 注册表由静态字符串和 JSON 基元构成，编码在构造时验证。
		schemas = append(schemas, provider.ToolSchema{Name: name, Description: definition.description, InputSchema: e.schemas[name]})
	}
	return schemas
}
func validateArguments(raw []byte, definition definition) (err error) {
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return domain.NewError(400, "INVALID_TOOL_INPUT", "Tool arguments must be an object.")
	}
	for name, value := range fields {
		if _, ok := definition.properties[name]; !ok || string(value) == "null" {
			return domain.NewError(400, "INVALID_TOOL_INPUT", "Unknown or null tool argument.")
		}
	}
	for _, name := range definition.required {
		if _, ok := fields[name]; !ok {
			return domain.NewError(400, "INVALID_TOOL_INPUT", "A required tool argument is missing.")
		}
	}
	return nil
}

func (e *Executor) fileAction(bot domain.Bot, name string, input fileInput, write bool) (policy.Action, error) {
	path, output, err := e.files.Describe(bot.ID, input.Path)
	if err != nil {
		return policy.Action{}, err
	}
	action := policy.Action{Class: "fs.read", Pattern: path, Risk: "low", Title: "读取工作区文件", Details: map[string]any{"tool": name, "path": path}}
	if write {
		action.Title = "修改工作区文件"
		action.Class = "fs.write_outside_out"
		action.Risk = "medium"
		if output {
			action.Class = "fs.write"
			action.Risk = "low"
		}
		action.Details["content_bytes"] = len(input.Content) + len(input.NewText)
	}
	return action, nil
}

func (e *Executor) invokeFile(ctx context.Context, botID, name string, input fileInput, audit workspace.BeforeWrite) (any, error) {
	switch name {
	case "file_read":
		return e.files.Read(ctx, botID, input.Path, input.StartLine, input.Limit)
	case "file_write":
		result, err := e.files.Write(ctx, botID, input.Path, input.Content, input.CreateDirs, audit)
		if err == nil {
			err = e.memories.FileChanged(ctx, botID, result.Path)
		}
		return result, err
	case "file_edit":
		result, err := e.files.Edit(ctx, botID, input.Path, input.OldText, input.NewText, audit)
		if err == nil {
			err = e.memories.FileChanged(ctx, botID, result.Path)
		}
		return result, err
	case "file_list":
		return e.files.List(ctx, botID, input.Path, input.Depth)
	case "file_glob":
		return e.files.Glob(ctx, botID, input.Path, input.Pattern)
	case "file_search":
		return e.files.Search(ctx, botID, input.Path, input.Pattern, input.Limit)
	}
	return nil, domain.NewError(400, "TOOL_NOT_ALLOWED", "This tool is not available.")
}
