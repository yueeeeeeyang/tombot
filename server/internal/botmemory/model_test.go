package botmemory

import (
	"strings"
	"testing"
)

func TestChangeValidation(t *testing.T) {
	base := Change{Key: "work.style", Category: "workflow", Summary: "先给结论", Content: "工作汇报先给结论。", Evidence: "先给结论"}
	if err := base.Validate(false); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Change){
		func(c *Change) { c.Key = "../other" }, func(c *Change) { c.Key = "*" }, func(c *Change) { c.ExpectedRevision = -1 },
		func(c *Change) { c.Category = "guess" }, func(c *Change) { c.Content = strings.Repeat("中", 342) },
		func(c *Change) { c.Summary = "" }, func(c *Change) { c.Evidence = "" }, func(c *Change) { c.Content = "正文\x00" },
		func(c *Change) { c.Summary = string([]byte{255}) },
	} {
		invalid := base
		mutate(&invalid)
		if err := invalid.Validate(false); err == nil {
			t.Fatal("非法记忆被接受")
		}
	}
	if err := base.Validate(true); err == nil {
		t.Fatal("删除操作接受了写入正文")
	}
	forget := Change{Key: "*", Evidence: "忘记全部记忆"}
	if err := forget.Validate(true); err != nil {
		t.Fatal(err)
	}
	forget.ExpectedRevision = 1
	if err := forget.Validate(true); err == nil {
		t.Fatal("全部遗忘接受了条目版本")
	}
}
