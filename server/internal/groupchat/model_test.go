package groupchat

import (
	"errors"
	"reflect"
	"testing"
)

func TestMentionsAndRecipients(t *testing.T) {
	members := []Member{{"a", "小黑"}, {"b", "小黑猫"}, {"c", "Alice"}}
	for _, test := range []struct {
		text    string
		ids     []string
		want    []string
		invalid bool
	}{
		{text: "@小黑 看看，@小黑猫：再检查 @用户。", want: []string{"a", "b", "user"}},
		{text: "me@Alice.example @陌生人 @小黑猫咪", want: []string{}},
		{text: "请检查", ids: []string{"b"}, want: []string{"b"}},
		{text: "@小黑 @小黑", want: []string{"a"}},
		{text: "错误目标", ids: []string{"outside"}, invalid: true},
	} {
		got, err := Mentions(test.text, members, test.ids)
		if test.invalid {
			if !errors.Is(err, ErrMention) {
				t.Fatal(err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, m := range got {
			ids = append(ids, m.ID)
		}
		if !reflect.DeepEqual(ids, test.want) {
			t.Fatalf("提及解析不一致：%s %v", test.text, ids)
		}
	}
	duplicate := []Member{{"a", "同名"}, {"b", "同名"}}
	if _, err := Mentions("@同名 请看", duplicate, nil); !errors.Is(err, ErrMention) {
		t.Fatal("歧义未拒绝", err)
	}
	if got, err := Mentions("@同名 请看", duplicate, []string{"b"}); err != nil || len(got) != 1 || got[0].ID != "b" {
		t.Fatal(got, err)
	}
	for _, test := range []struct {
		sender, reply string
		mentions      []Mention
		want          []string
	}{
		{sender: "user", want: []string{"a", "b", "c"}},
		{sender: "user", reply: "b", want: []string{"b"}},
		{sender: "a", want: []string{}},
		{sender: "a", mentions: []Mention{{ID: "a"}, {ID: "b"}, {ID: "user"}}, want: []string{"b"}},
		{sender: "user", mentions: []Mention{{ID: "user"}}, want: []string{}},
	} {
		if got := Recipients(members, test.mentions, test.reply, test.sender); !reflect.DeepEqual(got, test.want) {
			t.Fatal(got, test.want)
		}
	}
}
