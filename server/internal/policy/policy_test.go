package policy

import "testing"

func TestApprovalThresholds(t *testing.T) {
	for _, test := range []struct {
		strictness, risk string
		ask              bool
	}{{"loose", "low", false}, {"loose", "medium", false}, {"loose", "high", false}, {"standard", "low", false}, {"standard", "medium", false}, {"standard", "high", false}, {"strict", "low", false}, {"strict", "medium", false}, {"strict", "high", false}} {
		ask, err := NeedsApproval(test.strictness, test.risk)
		if err != nil || ask != test.ask {
			t.Fatalf("阈值错误 %+v %v", test, err)
		}
	}
	if _, err := NeedsApproval("unknown", "low"); err == nil {
		t.Fatal("未知严格度被接受")
	}
	if _, err := NeedsApproval("loose", "unknown"); err == nil {
		t.Fatal("未知风险默认放行")
	}
}
