package tools

import (
	"encoding/json"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/service"
)

func questionDefinition() definition {
	text := map[string]string{"type": "string"}
	return definition{"Ask the user a concise question in Chinese and wait for an actual answer. Never request passwords, credentials, or sensitive personal information. Supply up to 8 distinct suggested options, or an empty array for free text. This is a user interaction, not permission to perform another action: do not use it to bypass tool approvals. Returns {choice,text}; choice is null for free text. Skipped or expired questions return USER_NO_RESPONSE; never invent an answer. Default response window 24 hours, subject to cancellation, parent operation limits, and service restart.", map[string]any{"question": text, "options": map[string]any{"type": "array", "items": text, "maxItems": 8}}, []string{"question", "options"}, false}
}

func prepareQuestion(raw []byte) (preparedCall, error) {
	var input domain.UserQuestion
	if err := json.Unmarshal(raw, &input); err != nil {
		return preparedCall{}, domain.NewError(400, "INVALID_TOOL_INPUT", "Invalid question or options.")
	}
	input, err := service.NormalizeQuestion(input)
	if err != nil {
		return preparedCall{}, err
	}
	action := policy.Action{Class: "user.question", Pattern: "question", Risk: "low", Title: "向用户提问", ForceApproval: true, Details: map[string]any{"question": input.Question, "options": input.Options}}
	return preparedCall{action: action, input: input}, nil
}
