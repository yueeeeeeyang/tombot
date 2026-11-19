package computeragent

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/taskprocess"
)

// BrowserLimits 只接受受信节点提供的运行预算，不包含网址、程序、环境或 profile 路径。
type BrowserLimits struct {
	Width                int `json:"width"`
	Height               int `json:"height"`
	MaximumWindows       int `json:"maximum_windows"`
	MaximumPerSession    int `json:"maximum_per_session"`
	MaximumWindowRecords int `json:"maximum_window_records"`
	MaximumEvents        int `json:"maximum_events"`
	CallTimeoutMS        int `json:"call_timeout_ms"`
	DialogTimeoutMS      int `json:"dialog_timeout_ms"`
	ShutdownTimeoutMS    int `json:"shutdown_timeout_ms"`
}

// Validate 在时长换算前拒绝溢出，再复用共享浏览器的容量规则。
func (limits BrowserLimits) Validate() error {
	if limits.CallTimeoutMS < 100 || limits.CallTimeoutMS > 30000 || limits.DialogTimeoutMS < 1000 || limits.DialogTimeoutMS > 600000 || limits.ShutdownTimeoutMS < 1000 || limits.ShutdownTimeoutMS > 30000 {
		return errors.New("共享浏览器服务时限无效")
	}
	if limits.Width < 320 || limits.Width > 1280 || limits.Height < 240 || limits.Height > 800 || limits.MaximumWindows < 1 || limits.MaximumWindows > 128 || limits.MaximumPerSession < 1 || limits.MaximumPerSession > 32 || limits.MaximumPerSession > limits.MaximumWindows || limits.MaximumWindowRecords < limits.MaximumWindows || limits.MaximumWindowRecords > 4096 || limits.MaximumEvents < 16 || limits.MaximumEvents > 4096 {
		return errors.New("退役浏览器授权限制无效")
	}
	return nil
}

// BrowserGrant 的版本跨容器代次递增；服务实例、启动期限与预算共同组成不可改绑的授权。
type BrowserGrant struct {
	Identity Identity                   `json:"identity"`
	Revision int64                      `json:"revision"`
	Request  taskprocess.BrowserRequest `json:"request"`
	Limits   BrowserLimits              `json:"limits"`
}

// Validate 只校验不可变结构；查询和关闭允许引用已过期的启动期限，启动另行检查原始时钟。
func (grant BrowserGrant) Validate() error {
	if grant.Identity.validate() != nil || grant.Revision < 1 || grant.Request.Validate() != nil || grant.Limits.Validate() != nil {
		return errors.New("共享浏览器服务授权无效")
	}
	return nil
}

// BrowserSnapshot 保存最后消费版本与关闭屏障，不从 active 记录恢复浏览器执行。
type BrowserSnapshot struct {
	Version      int          `json:"version"`
	Grant        BrowserGrant `json:"grant"`
	Phase        string       `json:"phase"`
	StartupError string       `json:"startup_error,omitempty"`
}

// Validate 核验完整持久授权与阶段，供代理恢复和宿主目录回收共用。
func (snapshot BrowserSnapshot) Validate() error {
	if snapshot.Version != 1 || snapshot.Grant.Validate() != nil || snapshot.Phase != "consumed" && snapshot.Phase != "active" && snapshot.Phase != "closed" {
		return errors.New("共享浏览器持久服务记录无效")
	}
	if snapshot.StartupError != "" && (!validRetiredBrowserFailure(snapshot.StartupError) || snapshot.Phase == "active") {
		return errors.New("共享浏览器启动失败记录无效")
	}
	return nil
}

// BrowserStore 必须原子保存并刷盘；提交错误可能代表结果未知，监督器必须封闭实例。
type BrowserStore interface {
	LoadBrowser(context.Context) (*BrowserSnapshot, error)
	SaveBrowser(context.Context, BrowserSnapshot) error
}

// BrowserObservation 仅确认该实例的代理状态；active 不代表完整电脑或窗口客户端已验收。
type BrowserObservation struct {
	Grant BrowserGrant `json:"grant"`
	Phase string       `json:"phase"`
}

// validRetiredBrowserFailure 验证旧版磁盘记录中允许的启动故障分类。
func validRetiredBrowserFailure(code string) bool {
	switch code {
	case "browser_start_identity_failed", "browser_start_request_failed", "browser_start_directory_failed",
		"browser_start_pipe_failed", "browser_start_membership_failed", "browser_start_privileges_failed",
		"browser_start_launch_failed", "browser_start_group_failed", "browser_start_upload_failed",
		"browser_start_protocol_failed", "browser_start_sandbox_failed", "browser_start_profile_failed",
		"browser_start_process_exited", "browser_start_timeout", "browser_start_deadline_failed",
		"browser_start_budget_failed", "browser_start_state_failed":
		return true
	}
	return false
}
