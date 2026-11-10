package service

import (
	"io.yueyang/tom/internal/domain"
	"strings"
)

// ValidateUserMessage 统一约束工具输入和自动投递结果，避免写入桌面无法读取的长消息。
func ValidateUserMessage(content string) error {
	if strings.TrimSpace(content) == "" || len(content) > domain.MaxMessageBytes || utf16Len(content) > maxMessageLength {
		return domain.NewError(400, "INVALID_USER_MESSAGE", "A user message must contain 1 to 10000 UTF-16 units and at most 32768 UTF-8 bytes. Put longer results in a shared file and send a concise summary.")
	}
	return nil
}
