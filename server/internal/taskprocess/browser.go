package taskprocess

import (
	"errors"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/processgroup"
)

// BrowserRequest 仅用于识别并关闭升级前持久登记的共享服务。
type BrowserRequest struct {
	Version  int                           `json:"version"`
	Group    processgroup.ServiceReference `json:"group"`
	Deadline agentchannel.Deadline         `json:"deadline"`
}

func (request BrowserRequest) Validate() error {
	if request.Version != 1 || request.Group.Validate() != nil || request.Group.Kind != processgroup.BrowserService || request.Deadline.ExpiresNS <= 0 || request.Deadline.TimeNamespace == 0 {
		return errors.New("共享浏览器启动授权无效")
	}
	return nil
}
