package tools

import (
	"context"
	"encoding/json"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/service"
)

type routineInput struct {
	ID        string `json:"id"`
	Revision  int64  `json:"revision"`
	Name      string `json:"name"`
	Cron      string `json:"cron"`
	Prompt    string `json:"prompt"`
	IsEnabled *bool  `json:"is_enabled"`
	supplied  map[string]struct{}
}

func routineDefinitions() map[string]definition {
	text := map[string]string{"type": "string"}
	integer := map[string]string{"type": "integer"}
	boolean := map[string]string{"type": "boolean"}
	return map[string]definition{
		"routine_list":   {"List this bot's scheduled routines. Each item includes id, revision, name, cron, prompt and is_enabled. Use the revision when updating or deleting.", map[string]any{}, []string{}, false},
		"routine_upsert": {"Create or update one scheduled routine for this bot. It runs without waiting for approval. name, cron and prompt are required. cron uses five fields; the timezone is Asia/Shanghai. Omit id to create. To update, pass id and the current revision from routine_list, and repeat the fields you want to keep. is_enabled pauses or resumes the routine.", map[string]any{"id": text, "revision": integer, "name": text, "cron": text, "prompt": text, "is_enabled": boolean}, []string{"name", "cron", "prompt"}, true},
		"routine_delete": {"Delete one of this bot's scheduled routines immediately. id and revision must match routine_list.", map[string]any{"id": text, "revision": integer}, []string{"id", "revision"}, true},
	}
}

func (e *Executor) prepareRoutine(ctx context.Context, bot domain.Bot, name string, raw []byte) (call preparedCall, err error) {
	var input routineInput
	if err = json.Unmarshal(raw, &input); err != nil {
		return call, domain.NewError(400, "INVALID_TOOL_INPUT", "Invalid routine arguments.")
	}
	var supplied map[string]json.RawMessage
	if err = json.Unmarshal(raw, &supplied); err != nil {
		return call, err
	}
	input.supplied = map[string]struct{}{}
	for key := range supplied {
		input.supplied[key] = struct{}{}
	}
	action := policy.Action{Class: "routine.create", Pattern: bot.ID, Risk: "medium", Title: "查看定时任务", Details: map[string]any{"bot_id": bot.ID}}
	if name == "routine_list" {
		return preparedCall{action: action, timeout: 30 * time.Second, input: input}, nil
	}
	if input.ID != "" {
		current, getErr := e.routines.Get(ctx, input.ID)
		if getErr != nil {
			return call, getErr
		}
		if current.Target.BotID != bot.ID {
			return call, domain.NewError(403, "TOOL_NOT_ALLOWED", "This routine belongs to another bot.")
		}
	}
	if name == "routine_delete" {
		if input.ID == "" || input.Revision < 1 {
			return call, domain.NewError(400, "INVALID_TOOL_INPUT", "Deleting a routine requires its id and revision.")
		}
		action.Title = "删除定时任务"
		action.Pattern = input.ID
		return preparedCall{action: action, timeout: 30 * time.Second, input: input}, nil
	}
	if input.ID == "" && input.Revision != 0 {
		return call, domain.NewError(400, "INVALID_TOOL_INPUT", "A new routine cannot specify a revision.")
	}
	if input.ID != "" && input.Revision < 1 {
		return call, domain.NewError(400, "INVALID_TOOL_INPUT", "Updating a routine requires its current revision.")
	}
	action.Title = "保存定时任务"
	action.Details["name"] = input.Name
	return preparedCall{action: action, timeout: 30 * time.Second, input: input}, nil
}

func (e *Executor) invokeRoutine(ctx context.Context, botID, name string, input routineInput) (any, error) {
	actor := "bot:" + botID
	switch name {
	case "routine_list":
		rows, err := e.routines.List(ctx, botID, "", "", 50)
		if err != nil {
			return nil, err
		}
		listed := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			listed = append(listed, map[string]any{"id": row.ID, "revision": row.Revision, "name": row.Name, "cron": row.Cron, "prompt": row.Prompt, "is_enabled": row.IsEnabled})
		}
		return map[string]any{"routines": listed}, nil
	case "routine_delete":
		if err := e.routines.Delete(ctx, input.ID, input.Revision, actor); err != nil {
			return nil, err
		}
		return map[string]any{"deleted": true, "id": input.ID}, nil
	case "routine_upsert":
		if input.ID == "" {
			return e.routines.Commit(ctx, service.RoutineInput{RoutineDefinition: domain.RoutineDefinition{Name: input.Name, Cron: input.Cron, Prompt: input.Prompt, Target: domain.RoutineTarget{BotID: botID}}, IsEnabled: input.IsEnabled}, actor)
		}
		patch := service.RoutinePatch{Revision: input.Revision}
		if _, ok := input.supplied["name"]; ok {
			patch.Name = service.Field[string]{Set: true, Value: input.Name}
		}
		if _, ok := input.supplied["cron"]; ok {
			patch.Cron = service.Field[string]{Set: true, Value: input.Cron}
		}
		if _, ok := input.supplied["prompt"]; ok {
			patch.Prompt = service.Field[string]{Set: true, Value: input.Prompt}
		}
		if _, ok := input.supplied["is_enabled"]; ok {
			patch.IsEnabled = service.Field[*bool]{Set: true, Value: input.IsEnabled}
		}
		return e.routines.UpdateNow(ctx, input.ID, patch, actor)
	}
	return nil, domain.NewError(400, "TOOL_NOT_ALLOWED", "This routine tool is unavailable.")
}
