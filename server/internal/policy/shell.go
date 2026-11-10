package policy

import (
	"regexp"
	"strings"
)

var privilegedCommands = regexp.MustCompile(`(?i)(^|[\s;&|()])(?:[^\s;&|()]*/)?(sudo|doas|su|shutdown|reboot|halt|poweroff|mkfs(?:\.[a-z0-9]+)?|fdisk|parted|mount|umount)(?:[\s;&|()]|$)`)
var diskWrite = regexp.MustCompile(`(?i)(?:\bdd\b[^\n;&|]*\bof\s*=\s*['"]?/dev/|(?:>|\btee\b)[^\n;&|]*(?:/(?:etc|usr|bin|sbin|boot|System|Library|dev)(?:/|\b)))`)
var rootRemoval = regexp.MustCompile(`(?i)\brm\b[^\n;&|]*\s(?:['"]?/(?:['"]|\*|\s|$)|['"]?/(?:etc|usr|bin|sbin|boot|System|Library|dev)(?:/|['"]|\s|$))`)
var systemMutation = regexp.MustCompile(`(?i)\b(?:cp|mv|install|touch|mkdir|ln|chmod|chown|truncate)\b[^\n;&|]*\s['"]?/(?:etc|usr|bin|sbin|boot|System|Library|dev)(?:/|['"]|\s|$)`)
var inPlaceSystemEdit = regexp.MustCompile(`(?i)\b(?:sed|perl)\b[^\n;&|]*\s-[a-z]*i[^\n;&|]*\s['"]?/(?:etc|usr|bin|sbin|boot|System|Library|dev)(?:/|['"]|\s|$)`)

// ShellClass 是危险模式识别而非沙箱；命令仍受白名单、明确规则和审批约束。
func ShellClass(command string) string {
	normalized := strings.ReplaceAll(strings.ReplaceAll(command, "\\\n", ""), "\\", "")
	if privilegedCommands.MatchString(normalized) || diskWrite.MatchString(normalized) || rootRemoval.MatchString(normalized) || systemMutation.MatchString(normalized) || inPlaceSystemEdit.MatchString(normalized) {
		return "shell.privileged"
	}
	return "shell.exec"
}
