package tools

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/service"
)

type groupInput struct {
	Mentions []string `json:"mentions,omitempty"`
	GroupID  string   `json:"group_id"`
	Message  string   `json:"message"`
}

func groupDefinitions() map[string]definition {
	text := map[string]string{"type": "string"}
	return map[string]definition{
		"group_members": {"Read the names and IDs of equal active and archived members in a group you belong to. Does not read private prompts or memory.", map[string]any{"group_id": text}, []string{"group_id"}, false},
		"post_to_group": {"Post text in your current group turn. Optional mentions are exact member IDs or user for the administrator. Mentioned cats receive this message and can reply after you finish; never wait for them in this call. Without mentions the post inherits the current message audience. Only available during a group member run; never use this to return from a direct message into a group. Message limit: 16384 UTF-8 bytes; at most 64 public messages per run, including ordinary replies.", map[string]any{"group_id": text, "message": text, "mentions": map[string]any{"type": "array", "items": text, "maxItems": domain.MaxGroupMembers + 1}}, []string{"group_id", "message"}, false},
	}
}

func (e *Executor) prepareGroup(ctx context.Context, bot domain.Bot, name string, raw []byte) (call preparedCall, err error) {
	var input groupInput
	if err = json.Unmarshal(raw, &input); err != nil || input.GroupID == "" || len(input.GroupID) > 128 {
		return call, domain.NewError(400, "INVALID_TOOL_INPUT", "A group identifier is required.")
	}
	if name == "post_to_group" && (strings.TrimSpace(input.Message) == "" || len(input.Message) > domain.MaxGroupPostBytes) {
		return call, domain.NewError(400, "INVALID_TOOL_INPUT", "A group post must contain 1 to 16384 UTF-8 bytes.")
	}
	group, err := service.NewGroups(e.db, e.events).MemberGroup(ctx, input.GroupID, bot.ID)
	if err != nil {
		return call, err
	}
	action := policy.Action{Class: "group.read", Pattern: group.ID, Risk: "low", Title: "读取群组成员", Details: map[string]any{"group_id": group.ID, "群组": group.Name}}
	if name == "post_to_group" {
		action.Class = "group.post"
		action.Title = "在当前群组发言"
		action.Details["内容字节数"] = len(input.Message)
	}
	return preparedCall{action: action, timeout: 10 * time.Second, input: input, binding: strconv.FormatInt(group.Revision, 10)}, nil
}

func (e *Executor) invokeGroup(ctx context.Context, run domain.Run, callID, name string, input groupInput) (any, error) {
	groups := service.NewGroups(e.db, e.events)
	if name == "group_members" {
		group, err := groups.MemberGroup(ctx, input.GroupID, run.BotID)
		if err != nil {
			return nil, err
		}
		return group.Members, nil
	}
	message, err := groups.PostMessage(ctx, input.GroupID, callID, run, input.Message, input.Mentions...)
	if err != nil {
		return nil, err
	}
	return map[string]any{"message_id": message.ID, "turn_id": run.TurnID, "posted": true}, nil
}
