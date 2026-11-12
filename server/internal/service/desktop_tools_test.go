package service

import (
	"strings"
	"testing"
)

func TestDesktopCatsExposeMemoryAndRoutineTools(t *testing.T) {
	policy := desktopToolPolicy()
	names := map[string]bool{}
	for _, name := range policy.AllowedTools {
		names[name] = true
	}
	for _, name := range []string{"memory_search", "memory_write", "memory_delete", "routine_list", "routine_upsert", "routine_delete"} {
		if !names[name] {
			t.Fatalf("桌面工具策略缺少 %s", name)
		}
	}
	if policy.Approval != "loose" {
		t.Fatalf("桌面审批策略应为 loose，实际 %s", policy.Approval)
	}
	if !strings.Contains(desktopSoul, "memory_write") || !strings.Contains(desktopSoul, "routine_upsert") {
		t.Fatal(desktopSoul)
	}
}
