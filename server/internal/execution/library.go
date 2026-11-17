package execution

import (
	"context"
	"database/sql"

	"io.yueyang/tom/internal/chatlibrary"
	"io.yueyang/tom/internal/identity"
)

// ChatFiles 使用一致性快照读取目录及配额，不需要活动电脑会话。
func (service *Service) ChatFiles(ctx context.Context, principal identity.Principal, conversation, before, query string) (chatlibrary.Page, error) {
	var page chatlibrary.Page
	err := service.user(ctx, principal, false, func(tx *sql.Tx) error {
		var err error
		page, err = chatlibrary.List(ctx, tx, principal.UserID, conversation, before, query)
		return err
	})
	return page, err
}

// ChatFileContent 按已认证身份读取同会话文件，返回正文和元数据；不可用资源返回资料库错误。
func (service *Service) ChatFileContent(ctx context.Context, principal identity.Principal, conversation, id string) (chatlibrary.Content, error) {
	var content chatlibrary.Content
	err := service.user(ctx, principal, false, func(tx *sql.Tx) error {
		var err error
		content, err = chatlibrary.Download(ctx, tx, principal.UserID, conversation, id)
		return err
	})
	return content, err
}

// UploadChatFile 保存当前用户的聊天文件快照，以上传标识去重；超出配额或内容冲突时不写入。
func (service *Service) UploadChatFile(ctx context.Context, principal identity.Principal, conversation string, upload chatlibrary.Upload) (chatlibrary.File, error) {
	var file chatlibrary.File
	err := service.libraryWrite(ctx, principal, func(tx *sql.Tx) error {
		var err error
		file, err = chatlibrary.Save(ctx, tx, principal.UserID, conversation, upload)
		return err
	})
	return file, err
}

// RenameChatFile 按调用方读取的修订号改名，返回新元数据；旧版本不能覆盖并发变更。
func (service *Service) RenameChatFile(ctx context.Context, principal identity.Principal, conversation, id string, revision int64, name string) (chatlibrary.File, error) {
	var file chatlibrary.File
	err := service.libraryWrite(ctx, principal, func(tx *sql.Tx) error {
		var err error
		file, err = chatlibrary.Rename(ctx, tx, principal.UserID, conversation, id, revision, name)
		return err
	})
	return file, err
}

// DeleteChatFile 删除指定版本的聊天文件正文，返回权限、版本或存储错误，重复请求可安全重试。
func (service *Service) DeleteChatFile(ctx context.Context, principal identity.Principal, conversation, id string, revision int64) error {
	return service.libraryWrite(ctx, principal, func(tx *sql.Tx) error {
		return chatlibrary.Delete(ctx, tx, principal.UserID, conversation, id, revision)
	})
}

// libraryWrite 与聊天消息入队使用同一用户锁，避免删除与发送、并行上传配额检查竞争。
func (service *Service) libraryWrite(ctx context.Context, principal identity.Principal, apply func(*sql.Tx) error) error {
	return service.user(ctx, principal, true, func(tx *sql.Tx) error {
		if err := lockDispatch(ctx, tx, principal.UserID); err != nil {
			return err
		}
		return apply(tx)
	})
}

func chatFileBlocks(ctx context.Context, tx *sql.Tx, user, conversation string, ids []string) ([]Block, error) {
	blocks := []Block{}
	if len(ids) > 4 {
		return nil, ErrInvalidInput
	}
	if len(ids) == 0 {
		return blocks, nil
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return nil, ErrInvalidInput
		}
		seen[id] = true
		file, err := chatlibrary.Read(ctx, tx, user, conversation, id)
		if err != nil {
			return nil, err
		}
		if err = chatlibrary.Publish(ctx, tx, user, file); err != nil {
			return nil, err
		}
		blocks = append(blocks, Block{Kind: "chat_file", ChatFile: &file})
	}
	return blocks, nil
}
