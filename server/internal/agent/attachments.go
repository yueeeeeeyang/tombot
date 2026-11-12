package agent

import (
	"context"
	"strings"
	"time"

	"io.yueyang/tom/internal/domain"
)

// userBlocks 在持有调度锁前校验附件。单聊只接受自己收件箱的文件，多人会话只接受明确上传到共享区的文件。
func (r *Runtime) userBlocks(ctx context.Context, conversationID string, input SendInput) ([]domain.MessageBlock, error) {
	blocks := []domain.MessageBlock{}
	if input.Content != "" {
		blocks = append(blocks, domain.MessageBlock{Kind: "text", Text: input.Content})
	}
	if len(input.Attachments) == 0 {
		return blocks, nil
	}
	if len(input.Attachments) > domain.MaxMessageAttachments {
		return nil, domain.NewError(400, "ATTACHMENT_LIMIT", "Attach at most four files per message.")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var kind, owner string
	if err := r.deps.DB.QueryRowContext(ctx, "SELECT kind,COALESCE(bot_id,'') FROM conversations WHERE id=? AND is_archived=0", conversationID).Scan(&kind, &owner); err != nil {
		return nil, notFound(err)
	}
	shared := kind == "group" || kind == "bot_dm"
	if shared {
		owner = "user"
	}
	seen := make(map[string]bool)
	for _, item := range input.Attachments {
		if seen[item.Path] || shared != strings.HasPrefix(item.Path, "shared/") {
			return nil, domain.NewError(400, "ATTACHMENT_SCOPE", "Use unique uploads from this conversation's inbox scope.")
		}
		seen[item.Path] = true
		ref, err := r.deps.Files.Attachment(ctx, owner, item)
		if err != nil {
			return nil, err
		}
		ref.Content = nil
		blocks = append(blocks, domain.MessageBlock{Kind: "artifact", Artifact: &ref})
	}
	return blocks, nil
}

// contextArtifact 只允许用户附件和成功工具结果参与上下文装配。
func contextArtifact(block domain.MessageBlock) *domain.ArtifactRef {
	if block.Kind == "artifact" {
		return block.Artifact
	}
	if block.Kind == "tool_result" && block.ToolResult != nil && !block.ToolResult.IsError {
		return block.ToolResult.Artifact
	}
	return nil
}

// attachmentBudget 为内联图片和短文本预留预算，其他文件只发送已计入消息的元数据。
func attachmentBudget(ref domain.ArtifactRef, model domain.Model) (int, bool) {
	if ref.IsImage() && model.SupportsVision {
		return ref.VisionTokens(), true
	}
	if strings.HasPrefix(ref.MediaType, "text/") && ref.Bytes <= domain.MaxInlineTextBytes {
		return (ref.Bytes + 2) / 3, true
	}
	return 0, false
}
