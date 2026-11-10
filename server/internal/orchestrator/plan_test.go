package orchestrator

import (
	"io.yueyang/tom/internal/domain"
	"testing"
)

func TestPlanValidationRoutingAndDuplicateGuard(t *testing.T) {
	coordinator := "first"
	group := domain.Group{Routing: "mention", MaxSpeakers: 2, CoordinatorBotID: &coordinator, Members: []domain.GroupMember{{BotID: "first"}, {BotID: "second"}, {BotID: "third"}, {BotID: "archived", IsArchived: true}}}
	for _, encoded := range []string{
		`{"steps":[],"reason":null}`,
		`{"steps":null,"reason":"x"}`, `{"steps":[],"steps":[],"reason":"x"}`, `{"steps":[{"bot_id":"outsider","brief":"work"}],"reason":"x"}`,
		`{"steps":[{"bot_id":"archived","brief":"work"}],"reason":"x"}`, `{"steps":[{"bot_id":"first","brief":"a","brief":"b"}],"reason":"x"}`, `{"steps":[],"reason":"x"} trailing`,
	} {
		if _, err := ParsePlan(encoded, group); err == nil {
			t.Fatal("无效规划被接受", encoded)
		}
	}
	plan, err := ParsePlan(`{"steps":[{"bot_id":"first","brief":"Compare the new proposal with the complete documented requirements."},{"bot_id":"first","brief":"Compare the new proposal with the complete documented requirement."},{"bot_id":"first","brief":"Write a separate delivery checklist."},{"bot_id":"second","brief":"Review evidence."},{"bot_id":"third","brief":"Publish."}],"reason":"Roles match."}`, group)
	if err != nil {
		t.Fatal(err)
	}
	plan = Guard(plan, 2)
	if !plan.Clamped || len(plan.Steps) != 2 || len(plan.Steps[0].Brief) < 90 {
		t.Fatal("去重、合并或人数限制错误", plan)
	}
	plan = DirectPlan(group, "测试请求", nil)
	if len(plan.Steps) != 1 || plan.Steps[0].BotID != "first" {
		t.Fatal("无提及未交给协调者")
	}
	plan = DirectPlan(group, "测试请求", []string{"second"})
	if len(plan.Steps) != 1 || plan.Steps[0].BotID != "second" {
		t.Fatal("提及路由错误")
	}
	group.CoordinatorBotID = nil
	plan = DirectPlan(group, "测试请求", nil)
	if len(plan.Steps) != 0 {
		t.Fatal("无协调者应要求提及")
	}
	group.Routing = "all"
	plan = DirectPlan(group, "测试请求", nil)
	if len(plan.Steps) != 2 || !plan.Clamped {
		t.Fatal("全体路由未限制人数")
	}
}
