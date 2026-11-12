package computer

import (
	"testing"

	"io.yueyang/tom/internal/infra/ids"
)

func TestDesktopIdentity(t *testing.T) {
	valid := DesktopIdentity{UserID: ids.New(), ComputerID: ids.New(), BotID: ids.New(), DesktopID: ids.New(), UID: DesktopUIDBase + 1, GID: DesktopSharedGID, DisplayNumber: 1}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*DesktopIdentity){
		"缺少用户":  func(value *DesktopIdentity) { value.UserID = "" },
		"缺少电脑":  func(value *DesktopIdentity) { value.ComputerID = "" },
		"缺少机器人": func(value *DesktopIdentity) { value.BotID = "" },
		"缺少桌面":  func(value *DesktopIdentity) { value.DesktopID = "" },
		"系统身份":  func(value *DesktopIdentity) { value.UID = 0 },
		"非公共组":  func(value *DesktopIdentity) { value.GID++ },
		"保留显示":  func(value *DesktopIdentity) { value.DisplayNumber = 0 },
		"超出容量": func(value *DesktopIdentity) {
			value.DisplayNumber = MaxDesktops + 1
			value.UID = DesktopUIDBase + value.DisplayNumber
		},
		"错配显示": func(value *DesktopIdentity) { value.DisplayNumber++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed := valid
			change(&changed)
			if changed.Validate() == nil {
				t.Fatal("无效桌面身份被接受")
			}
		})
	}
	valid.DisplayNumber = MaxDesktops
	valid.UID = DesktopUIDBase + MaxDesktops
	if err := valid.Validate(); err != nil {
		t.Fatal("合法上界被拒绝", err)
	}
}
