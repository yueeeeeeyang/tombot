package processgroup

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/oklog/ulid/v2"
)

type ServiceKind string

const (
	ProbeService   ServiceKind = "probe"
	BrowserService ServiceKind = "browser"
)

// ServiceReference 绑定容器级基础服务，不借用 Bot、桌面、运行或任务租约。
// ID 由受信监督器为每次服务实例生成并持久化；已关闭的实例不能重新创建。
type ServiceReference struct {
	Container ContainerReference `json:"container"`
	ID        string             `json:"id"`
	Kind      ServiceKind        `json:"kind"`
}

func (reference ServiceReference) Validate() error {
	if err := reference.Container.Validate(); err != nil {
		return err
	}
	id, err := ulid.ParseStrict(reference.ID)
	if err != nil || id.String() != reference.ID || reference.Kind != ProbeService && reference.Kind != BrowserService {
		return errors.New("容器基础服务资源组身份无效")
	}
	return nil
}

// Name 将基础服务与 Bot 会话放入互不包含的同级目录，旧实例不能命中新实例。
func (reference ServiceReference) Name() string {
	return "tombot-service-" + string(reference.Kind) + "-" + strings.ToLower(reference.ID)
}

// ServiceGroups 只操作明确的基础服务实例；调用方先持久封闭新授权，再 Drain 并确认完成。
// 三个控制描述符依次为入组、整组终止和只读存活事件，不授予宿主目录权限。
type ServiceGroups interface {
	Create(context.Context, ServiceReference) error
	Controls(context.Context, ServiceReference) ([]*os.File, error)
	Contains(context.Context, ServiceReference, int) (bool, error)
	Drain(context.Context, ServiceReference) error
}
