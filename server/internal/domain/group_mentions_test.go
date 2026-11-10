package domain

import "testing"

func TestResolveGroupMentions(t *testing.T) {
	members := []GroupMember{{BotID: "a", Name: "小黑"}, {BotID: "b", Name: "小黑猫"}, {BotID: "c", Name: "Coding Boy"}}
	for _, test := range []struct {
		text string
		ids  []string
	}{{"@小黑猫 请看", []string{"b"}}, {"请@小黑 看一下", []string{"a"}}, {"@Coding Boy 看一下 @用户", []string{"c", "user"}}, {"@小黑 @小黑", []string{"a"}}, {"mail@example.test", []string{}}, {"无需提及", []string{}}} {
		actual, err := ResolveGroupMentions(test.text, members)
		if err != nil || len(actual) != len(test.ids) {
			t.Fatal(actual, err)
		}
		for i := range actual {
			if actual[i] != test.ids[i] {
				t.Fatal(actual, test.ids)
			}
		}
	}
	if _, err := ResolveGroupMentions("@不存在 请看", members); err == nil {
		t.Fatal("未知提及不应广播")
	}
	if _, err := ResolveGroupMentions("请@不存在 看一下", members); err == nil {
		t.Fatal("中文内的未知提及不应广播")
	}
	members = append(members, GroupMember{BotID: "duplicate", Name: "小黑"})
	if _, err := ResolveGroupMentions("@小黑 请看", members); err == nil {
		t.Fatal("同名成员不应自动猜测")
	}
}

func TestSelectedMentionDisambiguatesDuplicateNames(t *testing.T) {
	members := []GroupMember{{BotID: "a", Name: "猫"}, {BotID: "b", Name: "猫"}}
	ids, err := ResolveGroupMentions("@猫 你好", members, "b")
	if err != nil || len(ids) != 1 || ids[0] != "b" {
		t.Fatal(ids, err)
	}
}
