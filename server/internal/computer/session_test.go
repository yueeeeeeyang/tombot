package computer

import (
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/infra/ids"
)

func TestSessionCommand(t *testing.T) {
	valid := SessionCommand{Session: ExecutionSession{DesktopIdentity: DesktopIdentity{UserID: ids.New(), ComputerID: ids.New(), BotID: ids.New(), DesktopID: ids.New(), UID: DesktopUIDBase + 1, GID: DesktopSharedGID, DisplayNumber: 1}, NodeID: ids.New(), ContainerID: strings.Repeat("a", 64), Generation: 1, SessionID: ids.New(), RunID: ids.New(), FencingToken: 1}, Revision: 1, Lifetime: time.Second}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*SessionCommand){
		"缺少用户":   func(value *SessionCommand) { value.Session.UserID = "" },
		"缺少桌面":   func(value *SessionCommand) { value.Session.DesktopID = "" },
		"错误桌面身份": func(value *SessionCommand) { value.Session.UID++ },
		"错误容器":   func(value *SessionCommand) { value.Session.ContainerID = "container" },
		"大写容器":   func(value *SessionCommand) { value.Session.ContainerID = strings.Repeat("A", 64) },
		"缺少会话":   func(value *SessionCommand) { value.Session.SessionID = "" },
		"缺少任务":   func(value *SessionCommand) { value.Session.RunID = "" },
		"无控制代次":  func(value *SessionCommand) { value.Session.FencingToken = 0 },
		"无实例代次":  func(value *SessionCommand) { value.Session.Generation = 0 },
		"无版本":    func(value *SessionCommand) { value.Revision = 0 },
		"无界期限":   func(value *SessionCommand) { value.Lifetime = 6 * time.Minute },
		"过短期限":   func(value *SessionCommand) { value.Lifetime = time.Millisecond },
	} {
		t.Run(name, func(t *testing.T) {
			value := valid
			change(&value)
			if value.Validate() == nil {
				t.Fatal("无效命令被接受")
			}
		})
	}
	observation := SessionObservation{Session: valid.Session, Revision: valid.Revision, State: "drained"}
	if !observation.Matches(valid, "drained") || observation.Matches(valid, "active") {
		t.Fatal("状态匹配错误")
	}
	observation.Session.Generation++
	if observation.Matches(valid, "drained") {
		t.Fatal("旧实例证据被接受")
	}
	observation.Session = valid.Session
	observation.Revision++
	if observation.Matches(valid, "drained") {
		t.Fatal("错误控制版本被接受")
	}
}
