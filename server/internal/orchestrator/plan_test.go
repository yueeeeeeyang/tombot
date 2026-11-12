package orchestrator

import (
	"io.yueyang/tom/internal/domain"
	"testing"
)

func TestEqualMemberRoutingAndMentions(t *testing.T) {
	group := domain.Group{Members: []domain.GroupMember{{BotID: "a"}, {BotID: "b"}, {BotID: "c"}, {BotID: "archived", IsArchived: true}}}
	plan := DirectPlan(group, "你好", nil)
	if len(plan.Steps) != 3 {
		t.Fatal("未提及时应投递全部活跃成员", plan)
	}
	plan = DirectPlan(group, "指定回复", []string{"b", "b", "archived", "outsider"})
	if len(plan.Steps) != 1 || plan.Steps[0].BotID != "b" || plan.Steps[0].Brief != "指定回复" {
		t.Fatal("提及未限制到活跃成员或未去重", plan)
	}
	if len(DirectPlan(group, "给用户", []string{"user"}).Steps) != 0 {
		t.Fatal("提及用户不应触发猫咪")
	}
}
