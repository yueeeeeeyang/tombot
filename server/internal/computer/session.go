package computer

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/oklog/ulid/v2"
)

// ExecutionSession 是节点执行和回收的完整身份；它不包含通用容器权限或宿主路径。
type ExecutionSession struct {
	DesktopIdentity
	NodeID       string `json:"node_id"`
	ContainerID  string `json:"container_id"`
	Generation   int64  `json:"generation"`
	SessionID    string `json:"session_id"`
	RunID        string `json:"run_id"`
	FencingToken int64  `json:"fencing_token"`
}

var containerPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate 拒绝不完整身份，不能仅凭用户或容器标识清理资源。
func (session ExecutionSession) Validate() error {
	if err := session.DesktopIdentity.Validate(); err != nil {
		return err
	}
	for _, id := range []string{session.NodeID, session.SessionID, session.RunID} {
		if _, err := ulid.ParseStrict(id); err != nil {
			return errors.New("电脑执行会话标识无效")
		}
	}
	if !containerPattern.MatchString(session.ContainerID) || session.Generation < 1 || session.FencingToken < 1 {
		return errors.New("电脑执行实例或控制代次无效")
	}
	return nil
}

// SessionCommand 的版本随控制请求单调递增；节点必须持久拒绝旧版本及已关闭会话的重新开启。
// Lifetime 从节点受理时以单调时钟计时，失联后禁止新命令并回收进程。Close 不延长有效期。
type SessionCommand struct {
	Session  ExecutionSession `json:"session"`
	Revision int64            `json:"revision"`
	Lifetime time.Duration    `json:"lifetime_ns"`
}

func (command SessionCommand) Validate() error {
	if err := command.Session.Validate(); err != nil {
		return err
	}
	if command.Revision < 1 || command.Lifetime < time.Second || command.Lifetime > 5*time.Minute {
		return errors.New("电脑会话控制期限无效")
	}
	return nil
}

// SessionObservation 只报告同一命令的实际观测；drained 还要求已关闭所属桌面的输入并回收所属运行的进程、窗口和端口。
// 其他桌面与用户共享浏览器服务不能随单次运行被终止。
type SessionObservation struct {
	Session  ExecutionSession `json:"session"`
	Revision int64            `json:"revision"`
	State    string           `json:"state"`
}

func (observation SessionObservation) Matches(command SessionCommand, state string) bool {
	return observation.Session == command.Session && observation.Revision == command.Revision && observation.State == state
}

// SessionRuntime 由受信节点实现。Open 只准备会话，不执行用户动作；所有方法必须幂等。
// Close 是持久终止屏障：先封闭旧输入，再回收全部进程并验证为空；回包丢失后重复 Close 仍可确认 drained。
// Renew 不能创建、恢复或重新打开过期会话；迟到的 Open、Renew 和 Close 均不能影响另一代会话。
type SessionRuntime interface {
	Open(context.Context, SessionCommand) (SessionObservation, error)
	Renew(context.Context, SessionCommand) (SessionObservation, error)
	Close(context.Context, SessionCommand) (SessionObservation, error)
}
