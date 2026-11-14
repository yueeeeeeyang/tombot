package dockernode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
)

const MaximumImageArchive int64 = 16 << 30

// importArchive 在同一文件描述符上校验摘要并导入，不解压到宿主目录，也不向仓库回退。
func (preparer imagePreparer) importArchive(ctx context.Context, manifest ImageManifest) (result error) {
	if err := checkProtectedParents(filepath.Dir(manifest.ArchiveFile)); err != nil {
		return err
	}
	file, err := os.OpenFile(manifest.ArchiveFile, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return failure("image_archive_unavailable")
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	before, err := file.Stat()
	if err != nil {
		return failure("image_archive_stat_failed")
	}
	owner, ok := before.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || owner.Nlink != 1 || !before.Mode().IsRegular() || before.Mode().Perm() != 0600 || before.Size() < 1 || before.Size() > MaximumImageArchive {
		return failure("image_archive_unsafe")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, &contextReader{ctx: ctx, reader: file}); err != nil {
		return failure("image_archive_read_failed")
	}
	if hex.EncodeToString(hash.Sum(nil)) != manifest.ArchiveSHA256 {
		return failure("image_archive_digest_mismatch")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return failure("image_archive_seek_failed")
	}
	loader, ok := preparer.engine.(interface {
		loadImage(context.Context, io.Reader) error
	})
	if !ok {
		return failure("image_archive_import_unsupported")
	}
	if err := loader.loadImage(ctx, file); err != nil {
		return err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		return failure("image_archive_changed")
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

// loadImage 使用 Docker 的归档导入接口；结果不明确时不自动重发，调用方下次先 Inspect。
func (engine *dockerEngine) loadImage(ctx context.Context, archive io.Reader) (result error) {
	if _, ok := ctx.Deadline(); !ok {
		return failure("image_import_deadline_missing")
	}
	// HTTP 传输会关闭请求体，归档描述符仍由调用方持有以复核读取期间是否变化。
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker/v1.47/images/load?quiet=1", io.NopCloser(archive))
	if err != nil {
		return failure("image_import_request_invalid")
	}
	request.Header.Set("Content-Type", "application/x-tar")
	client := *engine.client
	client.Timeout = 0
	response, err := client.Do(request)
	if err != nil {
		return failure("image_import_result_unknown")
	}
	defer func() {
		if response.Body.Close() != nil {
			result = errors.Join(result, failure("image_import_close_failed"))
		}
	}()
	if response.StatusCode != http.StatusOK {
		return failure("image_import_rejected")
	}
	return readImageProgress(response.Body)
}
