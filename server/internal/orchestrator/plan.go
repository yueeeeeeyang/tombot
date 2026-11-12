// Package orchestrator 为平等群成员提供有界调度，不增加协调者或分工身份。
package orchestrator

import (
	"io.yueyang/tom/internal/domain"
	"slices"
)

// DirectPlan 未提及时投递所有活跃成员，提及时只投递指定成员，始终按群内顺序去重。
func DirectPlan(group domain.Group, content string, mentions []string) domain.GroupPlan {
	plan := domain.GroupPlan{Steps: []domain.GroupStep{}}
	for _, member := range group.Members {
		if !member.IsArchived && (len(mentions) == 0 || slices.Contains(mentions, member.BotID)) {
			plan.Steps = append(plan.Steps, domain.GroupStep{BotID: member.BotID, Brief: content})
		}
	}
	return plan
}
