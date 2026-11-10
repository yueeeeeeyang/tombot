package httpapi

import "testing"

func TestProvisionPhaseUsesChinese(t *testing.T) {
	cases := map[string]string{
		"":          "尚未开始",
		"pending":   "尚未开始",
		"detecting": "正在检查这台电脑",
		"preparing": "正在准备",
		"selfcheck": "正在准备",
		"ready":     "已就绪",
		"failed":    "准备失败",
		"other":     "进行中",
	}
	for phase, label := range cases {
		if got := provisionPhase(phase); got != label {
			t.Fatalf("阶段 %q 应显示 %q，实际 %q", phase, label, got)
		}
	}
}
