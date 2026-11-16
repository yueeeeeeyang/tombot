package computeragent

import (
	"bytes"
	"sync"

	"io.yueyang/tom/internal/browserprobe"
)

const maximumBrowserDiagnosticBytes = 64 << 10

// browserDiagnostics 仅保留有界启动输出用于匹配固定分类，绝不向调用方或日志返回原文。
// 达到上限后继续消费并丢弃输出，避免子进程因诊断管道阻塞。
type browserDiagnostics struct {
	mu     sync.Mutex
	prefix []byte
	closed bool
}

func (diagnostics *browserDiagnostics) Write(encoded []byte) (int, error) {
	diagnostics.mu.Lock()
	defer diagnostics.mu.Unlock()
	if diagnostics.closed {
		return len(encoded), nil
	}
	remaining := maximumBrowserDiagnosticBytes - len(diagnostics.prefix)
	diagnostics.prefix = append(diagnostics.prefix, encoded[:min(len(encoded), remaining)]...)
	return len(encoded), nil
}

func (diagnostics *browserDiagnostics) close() {
	diagnostics.mu.Lock()
	defer diagnostics.mu.Unlock()
	clear(diagnostics.prefix)
	diagnostics.prefix = nil
	diagnostics.closed = true
}

func (diagnostics *browserDiagnostics) failureCode() string {
	diagnostics.mu.Lock()
	defer diagnostics.mu.Unlock()
	for _, code := range []string{
		"browser_start_identity_failed", "browser_start_request_failed", "browser_start_directory_failed",
		"browser_start_pipe_failed", "browser_start_membership_failed", "browser_start_privileges_failed",
		"browser_start_launch_failed",
	} {
		if bytes.Contains(diagnostics.prefix, []byte((&browserprobe.Failure{Code: code}).Error())) {
			return code
		}
	}
	for _, signature := range []string{"No usable sandbox!", "Failed to move to new namespace", "Failed to unshare"} {
		if bytes.Contains(diagnostics.prefix, []byte(signature)) {
			return "browser_start_sandbox_failed"
		}
	}
	for _, signature := range []string{"Failed to create a ProcessSingleton", "profile appears to be in use"} {
		if bytes.Contains(diagnostics.prefix, []byte(signature)) {
			return "browser_start_profile_failed"
		}
	}
	return ""
}
