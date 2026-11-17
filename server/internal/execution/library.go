package execution

import (
	"context"
	"database/sql"

	"io.yueyang/tom/internal/grouplibrary"
	"io.yueyang/tom/internal/identity"
)

// GroupFiles 使用一致性快照读取目录及配额，不需要活动电脑会话。
func (service *Service) GroupFiles(ctx context.Context, principal identity.Principal, group, before, query string) (grouplibrary.Page, error) {
	var page grouplibrary.Page
	err := service.user(ctx, principal, false, func(tx *sql.Tx) error {
		var err error
		page, err = grouplibrary.List(ctx, tx, principal.UserID, group, before, query)
		return err
	})
	return page, err
}

// GroupFileContent 按已认证身份读取同群文件，返回正文和元数据；不可用资源返回资料库错误。
func (service *Service) GroupFileContent(ctx context.Context, principal identity.Principal, group, id string) (grouplibrary.Content, error) {
	var content grouplibrary.Content
	err := service.user(ctx, principal, false, func(tx *sql.Tx) error {
		var err error
		content, err = grouplibrary.Download(ctx, tx, principal.UserID, group, id)
		return err
	})
	return content, err
}

// UploadGroupFile 保存当前用户的群文件快照，以上传标识去重；超出配额或内容冲突时不写入。
func (service *Service) UploadGroupFile(ctx context.Context, principal identity.Principal, group string, upload grouplibrary.Upload) (grouplibrary.File, error) {
	var file grouplibrary.File
	err := service.libraryWrite(ctx, principal, func(tx *sql.Tx) error {
		var err error
		file, err = grouplibrary.Save(ctx, tx, principal.UserID, group, upload)
		return err
	})
	return file, err
}

// RenameGroupFile 按调用方读取的修订号改名，返回新元数据；旧版本不能覆盖并发变更。
func (service *Service) RenameGroupFile(ctx context.Context, principal identity.Principal, group, id string, revision int64, name string) (grouplibrary.File, error) {
	var file grouplibrary.File
	err := service.libraryWrite(ctx, principal, func(tx *sql.Tx) error {
		var err error
		file, err = grouplibrary.Rename(ctx, tx, principal.UserID, group, id, revision, name)
		return err
	})
	return file, err
}

// DeleteGroupFile 删除指定版本的群文件正文，返回权限、版本或存储错误，重复请求可安全重试。
func (service *Service) DeleteGroupFile(ctx context.Context, principal identity.Principal, group, id string, revision int64) error {
	return service.libraryWrite(ctx, principal, func(tx *sql.Tx) error { return grouplibrary.Delete(ctx, tx, principal.UserID, group, id, revision) })
}

// libraryWrite 与群消息入队使用同一用户锁，避免删除与发送、并行上传配额检查竞争。
func (service *Service) libraryWrite(ctx context.Context, principal identity.Principal, apply func(*sql.Tx) error) error {
	return service.user(ctx, principal, true, func(tx *sql.Tx) error {
		if err := lockDispatch(ctx, tx, principal.UserID); err != nil {
			return err
		}
		return apply(tx)
	})
}

func groupFileBlocks(ctx context.Context, tx *sql.Tx, user, conversation string, ids []string) ([]Block, error) {
	blocks := []Block{}
	if len(ids) > 4 {
		return nil, ErrInvalidInput
	}
	if len(ids) == 0 {
		return blocks, nil
	}
	var group string
	if err := tx.QueryRowContext(ctx, `SELECT group_id FROM tombot.conversations WHERE user_id=$1 AND id=$2 AND kind='group' AND NOT is_archived`, user, conversation).Scan(&group); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return nil, ErrInvalidInput
		}
		seen[id] = true
		file, err := grouplibrary.Read(ctx, tx, user, group, id)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, Block{Kind: "group_file", GroupFile: &file})
	}
	return blocks, nil
}
