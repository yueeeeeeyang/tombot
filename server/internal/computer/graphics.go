package computer

import (
	"context"
	"errors"
)

const DesktopWidth, DesktopHeight = 1280, 800

// DesktopStatus 仅报告当前授权的图形会话，不向调用者暴露设备、认证文件或连接地址。
type DesktopStatus struct {
	Session   ExecutionSession `json:"session"`
	State     string           `json:"state"`
	Width     int              `json:"width"`
	Height    int              `json:"height"`
	ErrorCode string           `json:"error_code,omitempty"`
}

func (status DesktopStatus) Validate() error {
	if status.Session.Validate() != nil {
		return errors.New("图形会话身份无效")
	}
	switch status.State {
	case "absent", "starting":
		if status.Width != 0 || status.Height != 0 || status.ErrorCode != "" {
			return errors.New("未就绪图形会话状态无效")
		}
	case "ready":
		if status.Width != DesktopWidth || status.Height != DesktopHeight || status.ErrorCode != "" {
			return errors.New("图形会话分辨率或状态无效")
		}
	case "failed":
		if status.Width != 0 || status.Height != 0 {
			return errors.New("失败图形会话不能报告可用画面")
		}
		switch status.ErrorCode {
		case "desktop_start_failed", "desktop_start_timeout", "desktop_readiness_failed", "desktop_exited":
		default:
			return errors.New("图形会话失败原因无效")
		}
	default:
		return errors.New("图形会话状态无效")
	}
	return nil
}

type DesktopRuntime interface {
	DesktopIORuntime
	StartDesktop(context.Context, ExecutionSession) (DesktopStatus, error)
	InspectDesktop(context.Context, ExecutionSession) (DesktopStatus, error)
}
