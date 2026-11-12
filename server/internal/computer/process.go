package computer

import (
	"errors"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

const MaximumProcessBytes = 64 << 10

// ProcessCommand 描述容器内普通用户程序，不包含 UID、资源组或任何宿主执行参数。
type ProcessCommand struct {
	WorkingDirectory string            `json:"working_directory"`
	Program          string            `json:"program"`
	Arguments        []string          `json:"arguments"`
	Environment      map[string]string `json:"environment"`
}

var processEnvironmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// Validate 校验绝对路径、内容上限与受管环境边界；错误不包含命令或环境原文。
func (command ProcessCommand) Validate() error {
	if !validAbsoluteDirectory(command.WorkingDirectory) || !validString(command.Program) || !path.IsAbs(command.Program) || path.Clean(command.Program) != command.Program || len(command.Program) > 4096 {
		return errors.New("任务引导目录或程序无效")
	}
	if len(command.Arguments) > 256 || len(command.Environment) > 128 {
		return errors.New("任务引导参数数量超过限制")
	}
	size := len(command.Program) + 1
	for _, argument := range command.Arguments {
		if !validString(argument) {
			return errors.New("任务引导参数无效")
		}
		size += len(argument) + 1
	}
	for name, value := range command.Environment {
		if !processEnvironmentName.MatchString(name) || !validString(value) {
			return errors.New("任务引导环境无效")
		}
		switch name {
		case "HOME", "TMPDIR", "TMP", "TEMP", "USER", "LOGNAME", "DISPLAY", "XAUTHORITY", "ICEAUTHORITY", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "NSS_WRAPPER_PASSWD", "NSS_WRAPPER_GROUP":
			return errors.New("任务引导环境不能覆盖受管目录或身份")
		}
		size += len(name) + len(value) + 2
	}
	if size > MaximumProcessBytes {
		return errors.New("任务引导参数内容超过限制")
	}
	return nil
}

func validString(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validAbsoluteDirectory(value string) bool {
	return value != "" && len(value) <= 4096 && validString(value) && path.IsAbs(value) && path.Clean(value) == value && !strings.ContainsAny(value, "\\\n\r")
}
