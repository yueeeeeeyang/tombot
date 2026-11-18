package computer

import (
	"context"
	"errors"
)

// ConsoleRequest 由受信服务构造；查看者身份和递增代次共同约束输入交接。
type ConsoleRequest struct {
	Session  ExecutionSession `json:"session"`
	ViewerID string           `json:"viewer_id"`
	Revision int64            `json:"revision"`
	Action   string           `json:"action"`
}

func (request ConsoleRequest) Validate() error {
	if (DesktopStream{Session: request.Session, ViewerID: request.ViewerID}).Validate() != nil || request.Revision < 1 {
		return errors.New("桌面接管身份或代次无效")
	}
	if request.Action != "prepare" && request.Action != "pause" && request.Action != "resume" {
		return errors.New("桌面接管动作无效")
	}
	return nil
}

type ConsoleReceipt struct {
	Session  ExecutionSession `json:"session"`
	Revision int64            `json:"revision"`
	State    string           `json:"state"`
}

func (receipt ConsoleReceipt) Validate(request ConsoleRequest) error {
	if request.Validate() != nil || receipt.Session != request.Session || receipt.Revision != request.Revision {
		return errors.New("桌面接管回执归属无效")
	}
	states := map[string]string{"prepare": "ready", "pause": "paused", "resume": "running"}
	if receipt.State != states[request.Action] {
		return errors.New("桌面接管控制未确认")
	}
	return nil
}

type ConsoleRuntime interface {
	Console(context.Context, ConsoleRequest) (ConsoleReceipt, error)
}
