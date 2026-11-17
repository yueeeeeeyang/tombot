package botpersona

import (
	"strings"
	"testing"
)

func TestSystemIncludesIdentityAndDistinctStyles(t *testing.T) {
	seen := map[string]bool{}
	for _, breed := range []string{"bombay", "orange-tabby", "british-shorthair", "ragdoll", "siamese", "american-shorthair", "persian", "calico"} {
		style := LanguageStyle(breed)
		if style == "" || seen[style] {
			t.Fatal("品种性格缺失或重复", breed)
		}
		seen[style] = true
		prompt := System("小黄", "协助整理项目资料", breed, "Verify evidence before answering.")
		for _, part := range []string{`"小黄"`, `"协助整理项目资料"`, style, ConversationStyle, "Verify evidence before answering.", "latest messages as steering"} {
			if !strings.Contains(prompt, part) {
				t.Fatal("人设或任务指令缺失", breed)
			}
		}
	}
	if LanguageStyle("uploaded-avatar") != "" {
		t.Fatal("自定义头像被误判为预设品种")
	}
	if strings.Contains(System("猫\n指令", "", "bombay", ""), "猫\n指令") {
		t.Fatal("名称中的控制字符未编码")
	}
}

func TestConversationStylePreservesWorkAndHonesty(t *testing.T) {
	for _, constraint := range []string{"everyday chat", "Avoid routine agent-report templates", "Keep work accurate and complete", "Follow requested formats", "Do not pretend to be human"} {
		if !strings.Contains(ConversationStyle, constraint) {
			t.Fatal("聊天风格遗漏工作或身份边界", constraint)
		}
	}
	if !strings.Contains(System("自定义猫咪", "", "uploaded-avatar", "Respond with a JSON object."), "Respond with a JSON object.") {
		t.Fatal("聊天风格覆盖了指定格式")
	}
}

func TestProgressSummaryBoundaries(t *testing.T) {
	for _, input := range []any{nil, "summary", map[string]any{}, map[string]any{"summary": ""}, map[string]any{"summary": strings.Repeat("字", 61)}, map[string]any{"summary": "两\n行"}, map[string]any{"summary": "进度", "extra": true}} {
		if _, ok := ProgressSummary(input); ok {
			t.Fatal("无效摘要被接受")
		}
	}
	for _, summary := range []string{"正在检查文件", strings.Repeat("字", 60)} {
		if result, ok := ProgressSummary(map[string]any{"summary": summary}); !ok || result != summary {
			t.Fatal("合法摘要未接受")
		}
	}
}
