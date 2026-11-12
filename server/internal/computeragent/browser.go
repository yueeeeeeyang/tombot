package computeragent

import (
	"context"
	"os"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/sharedbrowser"
	"io.yueyang/tom/internal/taskprocess"
)

// BrowserService 只暴露受限窗口操作和会话回收，不把全局 CDP 或进程控制入口交给桌面。
type BrowserService interface {
	sharedbrowser.Browser
	ReleaseSession(context.Context, computer.ExecutionSession) error
}

// BrowserScope 独占一个用户级 Chrome 服务组和运行目录，不属于任一 Bot 会话。
// Start 失败仍须 Drain；Done 表示共享能力结束，只有确认引导及整组进程退出后才能 Close。
type BrowserScope interface {
	Start(context.Context, taskprocess.BrowserRequest, sharedbrowser.Options) (BrowserService, error)
	Done() <-chan struct{}
	Revoke() error
	Drain(context.Context) error
	Close() error
}

// BrowserFactory 成功后接管三个服务组入口；grace 明确限定 Chrome 正常退出预算。
type BrowserFactory interface {
	OpenBrowser(taskprocess.BrowserRequest, []*os.File, time.Duration) (BrowserScope, error)
}
