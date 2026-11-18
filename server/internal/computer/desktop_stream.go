package computer

import (
	"context"
	"errors"
	"net"

	"github.com/oklog/ulid/v2"
)

// DesktopStream 将观看连接绑定到完整执行身份；客户端不能指定桌面地址。
type DesktopStream struct {
	Session  ExecutionSession `json:"session"`
	ViewerID string           `json:"viewer_id"`
}

func (request DesktopStream) Validate() error {
	id, err := ulid.ParseStrict(request.ViewerID)
	if request.Session.Validate() != nil || err != nil || id.String() != request.ViewerID {
		return errors.New("桌面观看身份无效")
	}
	return nil
}

type DesktopStreamRuntime interface {
	OpenDesktopStream(context.Context, DesktopStream) (net.Conn, error)
}
