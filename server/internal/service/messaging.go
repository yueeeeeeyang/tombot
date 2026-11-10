package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"strings"

	"io.yueyang/tom/internal/domain"
)

// ValidateMessaging 同时检查主动发信权、接收权与归档，不能仅靠调用方提示词约束。
func ValidateMessaging(sender, target domain.Bot) error {
	if sender.ID == target.ID {
		return domain.NewError(409, "BOT_MESSAGE_LOOP", "A bot cannot send a message to itself.")
	}
	if sender.IsArchived || !sender.MessagingPolicy.CanInitiate {
		return domain.NewError(403, "BOT_MESSAGE_NOT_ALLOWED", "The sending bot is not allowed to initiate messages.")
	}
	if target.IsArchived || !target.MessagingPolicy.AcceptDM {
		return domain.NewError(403, "BOT_DM_DENIED", "The target bot does not accept direct messages.")
	}
	return nil
}

// MessagingBinding 绑定审批看到的收件人身份和消息策略，哈希中不包含任何外部凭据。
func MessagingBinding(target domain.Bot) (string, error) {
	encoded, err := json.Marshal(struct {
		Name, Role, Soul string
		Model            *string
		Policy           domain.MessagingPolicy
		Archived         bool
	}{target.Name, target.RoleTitle, target.Soul, target.ModelID, target.MessagingPolicy, target.IsArchived})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// ValidateBotMessage 校验消息与显式共享上下文的容量；共享路径必须已归一化且不能逃逸。
func ValidateBotMessage(input domain.BotMessageRequest) error {
	if input.BotID == "" || len(input.BotID) > 128 || strings.TrimSpace(input.Message) == "" || len(input.Message) > domain.MaxMessageBytes || len(input.ContextFiles) > domain.MaxMessageContextFiles {
		return badInput("The bot message or shared context exceeds supported limits.")
	}
	total := 0
	seen := map[string]bool{}
	for _, file := range input.ContextFiles {
		if !SharedContextPath(file.Path) || seen[file.Path] {
			return badInput("Context files must be unique canonical paths under shared/.")
		}
		seen[file.Path] = true
		total += len(file.Content)
	}
	if total > domain.MaxMessageContextBytes {
		return badInput("The shared context exceeds 32768 UTF-8 bytes.")
	}
	return nil
}

func SharedContextPath(value string) bool {
	return strings.HasPrefix(value, "shared/") && value != "shared/" && path.Clean(value) == value && !strings.ContainsAny(value, "\\\x00") && len(value) <= 4096
}
