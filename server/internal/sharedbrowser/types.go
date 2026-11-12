// Package sharedbrowser 管理用户电脑内单一浏览器的共享配置与各 Bot 独立窗口。
package sharedbrowser

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/computer"
)

var (
	ErrInvalid     = errors.New("共享浏览器参数无效")
	ErrDenied      = errors.New("浏览器窗口不属于当前有效桌面")
	ErrCapacity    = errors.New("共享浏览器窗口容量已满")
	ErrUnavailable = errors.New("共享浏览器不可用")
	ErrUnknown     = errors.New("浏览器操作结果未确认，禁止重放")
	ErrConflict    = errors.New("浏览器操作序号或内容冲突")
	ErrDialog      = errors.New("浏览器窗口等待处理页面对话框")
	ErrBusy        = errors.New("浏览器窗口仍有在途动作，请查询状态")
)

// Authorizer 每次操作复验完整执行会话；必须包含当前控制代次、实例、租约及撤销状态。
type Authorizer func(context.Context, computer.ExecutionSession) error

type Options struct {
	UserID, ComputerID                               string
	Generation                                       int64
	Width, Height                                    int
	MaximumWindows, MaximumPerSession, MaximumEvents int
	MaximumWindowRecords                             int
	CallTimeout, DialogTimeout, ShutdownTimeout      time.Duration
	Authorize                                        Authorizer
	Logger                                           *slog.Logger
	Uploads                                          *UploadStore
}

// Validate 校验视口、容量、时限和完整会话授权；调用方应在启动进程前完成检查。
func (options Options) Validate() error {
	for _, id := range []string{options.UserID, options.ComputerID} {
		if _, err := ulid.ParseStrict(id); err != nil {
			return ErrInvalid
		}
	}
	if options.Generation < 1 || options.Authorize == nil || options.Logger == nil {
		return ErrInvalid
	}
	return options.ValidateLimits()
}

// ValidateLimits 供受信部署与服务授权复用同一容量、尺寸和时限规则，不需要构造用户或凭据。
func (options Options) ValidateLimits() error {
	if options.DialogTimeout < time.Second || options.DialogTimeout > 10*time.Minute {
		return ErrInvalid
	}
	if options.Width < 320 || options.Width > 1280 || options.Height < 240 || options.Height > 800 || options.MaximumWindows < 1 || options.MaximumWindows > 128 || options.MaximumPerSession < 1 || options.MaximumPerSession > 32 || options.MaximumPerSession > options.MaximumWindows || options.MaximumWindowRecords < options.MaximumWindows || options.MaximumWindowRecords > 4096 || options.MaximumEvents < 16 || options.MaximumEvents > 4096 || options.CallTimeout < 100*time.Millisecond || options.CallTimeout > 30*time.Second || options.ShutdownTimeout < time.Second || options.ShutdownTimeout > time.Minute {
		return ErrInvalid
	}
	return nil
}

type Window struct {
	ID           string `json:"id"`
	BotID        string `json:"bot_id"`
	DesktopID    string `json:"desktop_id"`
	ParentID     string `json:"parent_id,omitempty"`
	State        string `json:"state"`
	Sequence     int64  `json:"sequence"`
	CommandState string `json:"command_state,omitempty"`
	URL          string `json:"url"`
	Title        string `json:"title"`
	Notice       string `json:"notice,omitempty"`
	Dialog       Dialog `json:"dialog"`
}

// Dialog 的序号只在所属窗口内有效，不暴露框架或协议会话标识。
type Dialog struct {
	ID            int64  `json:"id"`
	Kind          string `json:"kind"`
	Message       string `json:"message"`
	DefaultPrompt string `json:"default_prompt"`
}

type DialogReply struct {
	ID     int64  `json:"id"`
	Accept bool   `json:"accept"`
	Text   string `json:"text"`
}

// Address 不接受 CDP target、session、调试地址或原始浏览器上下文。
type Address struct {
	Session  computer.ExecutionSession
	WindowID string
}

// Command 是完整的一次用户动作；没有跨请求保持按住的输入，序号由受信调用方持久分配。
type Command struct {
	Sequence  int64  `json:"sequence"`
	Kind      string `json:"kind"`
	URL       string `json:"url,omitempty"`
	Text      string `json:"text,omitempty"`
	Key       string `json:"key,omitempty"`
	Modifiers int64  `json:"modifiers,omitempty"`
	X         int    `json:"x,omitempty"`
	Y         int    `json:"y,omitempty"`
	DeltaX    int    `json:"delta_x,omitempty"`
	DeltaY    int    `json:"delta_y,omitempty"`
}

func validText(value string, maximum int) bool {
	return len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsFunc(value, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' })
}
func navigationURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && len(value) <= 8192 && !strings.ContainsAny(value, "\\\r\n\t") && strings.TrimSpace(value) == value && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Hostname() != "" && parsed.User == nil && parsed.Opaque == ""
}

// Validate 复用网关与本地工具的动作边界，拒绝多余动作参数。
func (command Command) Validate(width, height int) error {
	if command.Sequence < 1 {
		return ErrInvalid
	}
	remaining := command
	remaining.Sequence, remaining.Kind = 0, ""
	switch command.Kind {
	case "navigate":
		if !navigationURL(command.URL) {
			return ErrInvalid
		}
		remaining.URL = ""
	case "back", "forward", "reload":
	case "type":
		if command.Text == "" || !validText(command.Text, 4096) {
			return ErrInvalid
		}
		remaining.Text = ""
	case "key":
		if _, ok := keyCodes[command.Key]; !ok || command.Modifiers < 0 || command.Modifiers > 15 {
			return ErrInvalid
		}
		remaining.Key, remaining.Modifiers = "", 0
	case "click", "double_click", "move", "scroll":
		if command.X < 0 || command.X >= width || command.Y < 0 || command.Y >= height {
			return ErrInvalid
		}
		remaining.X, remaining.Y = 0, 0
		if command.Kind == "scroll" {
			if command.DeltaX < -2000 || command.DeltaX > 2000 || command.DeltaY < -2000 || command.DeltaY > 2000 || command.DeltaX == 0 && command.DeltaY == 0 {
				return ErrInvalid
			}
			remaining.DeltaX, remaining.DeltaY = 0, 0
		}
	default:
		return ErrInvalid
	}
	if remaining != (Command{}) {
		return ErrInvalid
	}
	return nil
}

var keyCodes = map[string]int64{"Enter": 13, "Tab": 9, "Escape": 27, "Backspace": 8, "Delete": 46, "Home": 36, "End": 35, "PageUp": 33, "PageDown": 34, "ArrowLeft": 37, "ArrowUp": 38, "ArrowRight": 39, "ArrowDown": 40, "A": 65, "C": 67, "L": 76, "V": 86, "X": 88, "Z": 90}
