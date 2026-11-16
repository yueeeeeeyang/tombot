package computeragent

import (
	"bytes"
	"testing"

	"io.yueyang/tom/internal/browserprobe"
)

func TestBrowserDiagnosticsClassifiesOnlyKnownFailures(t *testing.T) {
	for _, test := range []struct{ output, code string }{
		{(&browserprobe.Failure{Code: "browser_start_membership_failed"}).Error(), "browser_start_membership_failed"},
		{"Failed to move to new namespace: Operation not permitted", "browser_start_sandbox_failed"},
		{"No usable sandbox!", "browser_start_sandbox_failed"},
		{"Failed to create a ProcessSingleton for your profile directory", "browser_start_profile_failed"},
		{"页面内容或未知错误", ""},
		{"browser_start_unknown", ""},
	} {
		t.Run(test.code+test.output[:3], func(t *testing.T) {
			diagnostics := &browserDiagnostics{}
			for _, character := range []byte(test.output) {
				if count, err := diagnostics.Write([]byte{character}); err != nil || count != 1 {
					t.Fatal(count, err)
				}
			}
			if code := diagnostics.failureCode(); code != test.code {
				t.Fatal("启动分类不符", code)
			}
			diagnostics.close()
			if len(diagnostics.prefix) != 0 || diagnostics.failureCode() != "" {
				t.Fatal("结束后仍保留原始输出")
			}
			if _, err := diagnostics.Write([]byte(test.output)); err != nil || len(diagnostics.prefix) != 0 {
				t.Fatal("启动结束后仍收集诊断")
			}
		})
	}
}

func TestBrowserDiagnosticsBoundsOutputWithoutBlockingWriter(t *testing.T) {
	diagnostics := &browserDiagnostics{}
	encoded := bytes.Repeat([]byte("x"), maximumBrowserDiagnosticBytes*2)
	for range 3 {
		if count, err := diagnostics.Write(encoded); err != nil || count != len(encoded) {
			t.Fatal("输出满后阻塞子进程", count, err)
		}
	}
	if len(diagnostics.prefix) != maximumBrowserDiagnosticBytes {
		t.Fatal("诊断没有大小限制")
	}
	if _, err := diagnostics.Write([]byte("No usable sandbox!")); err != nil {
		t.Fatal(err)
	}
	if diagnostics.failureCode() != "" {
		t.Fatal("超过启动诊断预算仍解析输出")
	}
}
