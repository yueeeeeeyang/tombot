package computer

import (
	"context"
	"errors"
	"regexp"
)

const (
	MaximumDesktopImageBytes = 5 << 20
	MaximumDesktopChunkBytes = 32 << 10
	MaximumDesktopEvents     = 512
)

// DesktopCapture 使用持久操作的递增序号读取不可变画面；序号可跳过其他操作，重复序号不能重新截图。
type DesktopCapture struct {
	Session  ExecutionSession `json:"session"`
	Sequence int64            `json:"sequence"`
}

func (request DesktopCapture) Validate() error {
	if request.Session.Validate() != nil || request.Sequence < 1 {
		return errors.New("桌面截图会话或序号无效")
	}
	return nil
}

type DesktopImage struct {
	DesktopCapture
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	MediaType string `json:"media_type"`
	Size      int    `json:"size"`
	SHA256    string `json:"sha256"`
}

var desktopDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (result DesktopImage) Validate(request DesktopCapture) error {
	if request.Validate() != nil || result.DesktopCapture != request || result.Width != DesktopWidth || result.Height != DesktopHeight || result.MediaType != "image/png" || result.Size < 1 || result.Size > MaximumDesktopImageBytes || !desktopDigestPattern.MatchString(result.SHA256) {
		return errors.New("桌面截图归属、格式或范围无效")
	}
	return nil
}

type DesktopImageRead struct {
	DesktopCapture
	Offset       int `json:"offset"`
	MaximumBytes int `json:"maximum_bytes"`
}

func (request DesktopImageRead) Validate() error {
	if request.DesktopCapture.Validate() != nil || request.Offset < 0 || request.Offset > MaximumDesktopImageBytes || request.MaximumBytes < 1 || request.MaximumBytes > MaximumDesktopChunkBytes {
		return errors.New("桌面截图读取范围无效")
	}
	return nil
}

type DesktopImageChunk struct {
	Image  DesktopImage `json:"image"`
	Offset int          `json:"offset"`
	Bytes  []byte       `json:"bytes"`
}

func (chunk DesktopImageChunk) Validate(request DesktopImageRead) error {
	if request.Validate() != nil || chunk.Image.Validate(request.DesktopCapture) != nil || chunk.Offset != request.Offset || request.Offset > chunk.Image.Size || len(chunk.Bytes) != min(request.MaximumBytes, chunk.Image.Size-request.Offset) {
		return errors.New("桌面截图分块不完整或归属不符")
	}
	return nil
}

// DesktopEvent 表示 X11 keysym 或固定画面内的鼠标事件，不接受显示编号或连接路径。
type DesktopEvent struct {
	Kind    string `json:"kind"`
	Key     uint32 `json:"key,omitempty"`
	Down    bool   `json:"down,omitempty"`
	X       int    `json:"x,omitempty"`
	Y       int    `json:"y,omitempty"`
	Buttons uint8  `json:"buttons,omitempty"`
}

// DesktopInput 是完整手势；所有按键和鼠标按钮须在同一批次释放，禁止跨请求保留按住状态。
type DesktopInput struct {
	Session  ExecutionSession `json:"session"`
	Sequence int64            `json:"sequence"`
	Events   []DesktopEvent   `json:"events"`
}

func (request DesktopInput) Validate() error {
	if request.Session.Validate() != nil || request.Sequence < 1 || len(request.Events) < 1 || len(request.Events) > MaximumDesktopEvents {
		return errors.New("桌面输入会话、序号或事件数量无效")
	}
	pressed := make(map[uint32]bool)
	var buttons uint8
	for _, event := range request.Events {
		switch event.Kind {
		case "key":
			if !validDesktopKey(event.Key) || event.X != 0 || event.Y != 0 || event.Buttons != 0 || pressed[event.Key] == event.Down {
				return errors.New("桌面键盘事件无效或按下释放不匹配")
			}
			if event.Down {
				pressed[event.Key] = true
			} else {
				delete(pressed, event.Key)
			}
		case "pointer":
			if event.Key != 0 || event.Down || event.X < 0 || event.X >= DesktopWidth || event.Y < 0 || event.Y >= DesktopHeight {
				return errors.New("桌面鼠标事件或坐标无效")
			}
			buttons = event.Buttons
		default:
			return errors.New("桌面输入事件类型无效")
		}
	}
	if len(pressed) != 0 || buttons != 0 {
		return errors.New("桌面输入必须释放全部按键和鼠标按钮")
	}
	return nil
}

func validDesktopKey(key uint32) bool {
	if key >= 0x20 && key <= 0x7e || key >= 0xa0 && key <= 0xff || key >= 0xffbe && key <= 0xffc9 {
		return true
	}
	if key >= 0x01000100 && key <= 0x0110ffff {
		return key < 0x0100d800 || key > 0x0100dfff
	}
	switch key {
	case 0xff08, 0xff09, 0xff0d, 0xff1b, 0xff50, 0xff51, 0xff52, 0xff53, 0xff54, 0xff55, 0xff56, 0xff57, 0xff63, 0xffff,
		0xffe1, 0xffe2, 0xffe3, 0xffe4, 0xffe7, 0xffe8, 0xffe9, 0xffea, 0xffeb, 0xffec:
		return true
	}
	return false
}

// DesktopInputReceipt 的 sent 仅表示完整写入桌面连接，不保证应用已处理；uncertain 禁止自动重放。
type DesktopInputReceipt struct {
	Session  ExecutionSession `json:"session"`
	Sequence int64            `json:"sequence"`
	State    string           `json:"state"`
}

func (receipt DesktopInputReceipt) Validate(request DesktopInput) error {
	if request.Validate() != nil || receipt.Session != request.Session || receipt.Sequence != request.Sequence || receipt.State != "sent" && receipt.State != "uncertain" {
		return errors.New("桌面输入回执身份或状态无效")
	}
	return nil
}

type DesktopIORuntime interface {
	CaptureDesktop(context.Context, DesktopCapture) (DesktopImage, error)
	ReadDesktopImage(context.Context, DesktopImageRead) (DesktopImageChunk, error)
	InputDesktop(context.Context, DesktopInput) (DesktopInputReceipt, error)
}
