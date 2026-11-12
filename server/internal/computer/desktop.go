package computer

import (
	"errors"

	"github.com/oklog/ulid/v2"
)

// 桌面与基础服务分别使用镜像保留的数值空间，不复用系统账户或彼此的身份。
// 每个用户有独立容器，因此公共 GID 可以相同，不代表跨用户共享文件。
const (
	DesktopSharedGID      = 20000
	BrowserUID            = 30000
	ProbeUID              = 30001
	DesktopUIDBase        = 100000
	MaxDesktops           = 4095
	MaxConcurrentDesktops = 64
)

// DesktopIdentity 是跨容器重建保持稳定的逻辑身份；它不代表正在运行或已取得输入控制权。
// UID 保护桌面认证和屏幕，公共 GID 用于共享业务目录；编号由服务端分配。
type DesktopIdentity struct {
	UserID        string `json:"user_id"`
	ComputerID    string `json:"computer_id"`
	BotID         string `json:"bot_id"`
	DesktopID     string `json:"desktop_id"`
	UID           int    `json:"linux_uid"`
	GID           int    `json:"shared_gid"`
	DisplayNumber int    `json:"display_number"`
}

// Validate 拒绝无效身份和保留编号以外的映射，不能用客户端传入的 UID 或 DISPLAY 替代登记记录。
func (identity DesktopIdentity) Validate() error {
	for _, id := range []string{identity.UserID, identity.ComputerID, identity.BotID, identity.DesktopID} {
		if _, err := ulid.ParseStrict(id); err != nil {
			return errors.New("机器人桌面标识无效")
		}
	}
	if identity.DisplayNumber < 1 || identity.DisplayNumber > MaxDesktops || identity.UID != DesktopUIDBase+identity.DisplayNumber || identity.GID != DesktopSharedGID {
		return errors.New("机器人桌面运行身份无效")
	}
	return nil
}
