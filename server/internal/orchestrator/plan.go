// Package orchestrator 只负责群路由、分工与护栏，通过执行端口复用 Agent 运行时。
package orchestrator

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"

	"io.yueyang/tom/internal/domain"
)

func object(encoded []byte, keys ...string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(encoded))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("规划必须是对象")
	}
	allowed := map[string]bool{}
	for _, key := range keys {
		allowed[key] = true
	}
	values := map[string]json.RawMessage{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok || !allowed[key] || values[key] != nil {
			return nil, errors.New("规划包含未知或重复字段")
		}
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return nil, err
		}
		values[key] = value
	}
	if _, err = d.Token(); err != nil {
		return nil, err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("规划包含尾随内容")
	}
	if len(values) != len(keys) {
		return nil, errors.New("规划缺少必要字段")
	}
	return values, nil
}

// ParsePlan 严格解析路由输出，拒绝越权成员和无界文本；数量裁剪由 Guard 统一执行。
func ParsePlan(encoded string, group domain.Group) (domain.GroupPlan, error) {
	plan := domain.GroupPlan{Steps: []domain.GroupStep{}}
	if len(encoded) > 32<<10 {
		return plan, errors.New("规划输出超过容量")
	}
	root, err := object([]byte(encoded), "steps", "reason")
	if err != nil {
		return plan, err
	}
	var steps []json.RawMessage
	if err = json.Unmarshal(root["steps"], &steps); err != nil || steps == nil || len(steps) > domain.MaxGroupMembers {
		return plan, errors.New("规划步骤无效")
	}
	if err = json.Unmarshal(root["reason"], &plan.Reason); err != nil || bytes.Equal(root["reason"], []byte("null")) || len(plan.Reason) > 2000 {
		return plan, errors.New("规划理由无效")
	}
	allowed := map[string]bool{}
	for _, member := range group.Members {
		allowed[member.BotID] = !member.IsArchived
	}
	for _, encoded := range steps {
		fields, err := object(encoded, "bot_id", "brief")
		if err != nil {
			return plan, err
		}
		var step domain.GroupStep
		if json.Unmarshal(fields["bot_id"], &step.BotID) != nil || json.Unmarshal(fields["brief"], &step.Brief) != nil {
			return plan, errors.New("规划步骤类型无效")
		}
		step.Brief = strings.TrimSpace(step.Brief)
		if !allowed[step.BotID] || step.Brief == "" || len(step.Brief) > 8192 {
			return plan, errors.New("规划成员或任务无效")
		}
		plan.Steps = append(plan.Steps, step)
	}
	return plan, nil
}

func similar(first, second string) bool {
	normalize := func(value string) []rune {
		return []rune(strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return unicode.ToLower(r)
		}, value))
	}
	a, b := normalize(first), normalize(second)
	if string(a) == string(b) {
		return true
	}
	if len(a) < 2 || len(b) < 2 {
		return false
	}
	counts := map[[2]rune]int{}
	for i := 1; i < len(a); i++ {
		counts[[2]rune{a[i-1], a[i]}]++
	}
	common := 0
	for i := 1; i < len(b); i++ {
		pair := [2]rune{b[i-1], b[i]}
		if counts[pair] > 0 {
			common++
			counts[pair]--
		}
	}
	return float64(2*common)/float64(len(a)+len(b)-2) > .9
}

// Guard 去重同成员的近似任务，并将同成员的不同分工合并到一次执行。
func Guard(plan domain.GroupPlan, maximum int) domain.GroupPlan {
	steps := []domain.GroupStep{}
	original := map[string][]string{}
	positions := map[string]int{}
	for _, step := range plan.Steps {
		duplicate := false
		for _, brief := range original[step.BotID] {
			if similar(brief, step.Brief) {
				duplicate = true
				break
			}
		}
		if duplicate {
			plan.Clamped = true
			continue
		}
		if position, exists := positions[step.BotID]; exists {
			if len(steps[position].Brief)+len(step.Brief)+32 > 16<<10 {
				plan.Clamped = true
				continue
			}
			steps[position].Brief += "\nAdditional assignment:\n" + step.Brief
			original[step.BotID] = append(original[step.BotID], step.Brief)
			continue
		}
		if len(steps) >= maximum {
			plan.Clamped = true
			continue
		}
		positions[step.BotID] = len(steps)
		original[step.BotID] = append(original[step.BotID], step.Brief)
		steps = append(steps, step)
	}
	plan.Steps = steps
	return plan
}

func DirectPlan(group domain.Group, content string, mentions []string) domain.GroupPlan {
	plan := domain.GroupPlan{Steps: []domain.GroupStep{}}
	if group.Routing == "mention" {
		if len(mentions) == 0 && group.CoordinatorBotID != nil {
			mentions = []string{*group.CoordinatorBotID}
		}
		for _, id := range mentions {
			for _, member := range group.Members {
				if member.BotID == id && !member.IsArchived {
					plan.Steps = append(plan.Steps, domain.GroupStep{BotID: id, Brief: content})
				}
			}
		}
	} else {
		for _, member := range group.Members {
			if !member.IsArchived {
				plan.Steps = append(plan.Steps, domain.GroupStep{BotID: member.BotID, Brief: content})
			}
		}
	}
	return Guard(plan, group.MaxSpeakers)
}
