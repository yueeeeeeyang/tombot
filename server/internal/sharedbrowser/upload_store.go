package sharedbrowser

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

// 上传额度属于本机协议上限；用户仍受容器内存和磁盘的总额度约束。
const MaximumUploadBytes = 8 << 20
const MaximumUploadFiles = 8
const maximumUploadBody = 12 << 20
const maximumUploadStorage = 64 << 20
const maximumUploadBatches = 128

type UploadFile struct {
	Name    string `json:"name"`
	Content []byte `json:"content"`
}
type UploadRequest struct {
	Sequence int64        `json:"sequence"`
	Selector string       `json:"selector"`
	Files    []UploadFile `json:"files"`
}

func (request UploadRequest) Validate() error {
	if request.Sequence < 1 || request.Selector == "" || !validText(request.Selector, 4096) || len(request.Files) < 1 || len(request.Files) > MaximumUploadFiles {
		return ErrInvalid
	}
	seen := map[string]bool{}
	total := 0
	for _, file := range request.Files {
		if file.Name == "" || file.Name == "." || file.Name == ".." || !validText(file.Name, 255) || strings.ContainsAny(file.Name, "/\\\r\n\t") || seen[file.Name] {
			return ErrInvalid
		}
		seen[file.Name] = true
		if len(file.Content) > MaximumUploadBytes-total {
			return ErrCapacity
		}
		total += len(file.Content)
	}
	return nil
}

// UploadStore 只保存调用者已经读出的内容，不接收或读取任何业务文件路径。
// 临时副本不可由 Bot 修改，保留到所属页面关闭，避免页面延迟读取 File 对象时丢失内容。
type UploadStore struct {
	mu        sync.Mutex
	directory string
	batches   map[string]uploadBatch
	bytes     int
	closed    bool
	failure   error
}
type uploadBatch struct {
	paths []string
	bytes int
}

// NewUploadStore 由容器内 root 监督器创建独占临时目录，失败不返回可用存储。
func NewUploadStore() (*UploadStore, error) {
	if os.Geteuid() != 0 {
		return nil, ErrDenied
	}
	directory, err := os.MkdirTemp("/tmp", "tombot-browser-uploads-")
	if err != nil {
		return nil, ErrUnavailable
	}
	if err := os.Chmod(directory, 0711); err != nil {
		return nil, errors.Join(ErrUnavailable, os.Remove(directory))
	}
	return &UploadStore{directory: directory, batches: map[string]uploadBatch{}}, nil
}

// Stage 返回内部批次标识与 Chrome 可读路径；容量、取消或写入失败时回收本次副本。
func (store *UploadStore) Stage(ctx context.Context, files []UploadFile) (string, []string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed || store.failure != nil {
		return "", nil, ErrUnavailable
	}
	if err := (UploadRequest{Sequence: 1, Selector: "input", Files: files}).Validate(); err != nil {
		return "", nil, err
	}
	total := 0
	for _, file := range files {
		total += len(file.Content)
	}
	if len(store.batches) >= maximumUploadBatches || total > maximumUploadStorage-store.bytes {
		return "", nil, ErrCapacity
	}
	id := ids.New()
	directory := filepath.Join(store.directory, id)
	if err := os.Mkdir(directory, 0700); err != nil {
		return "", nil, ErrUnavailable
	}
	batch := uploadBatch{}
	// 先登记目录，失败清理只移除本次已创建的文件，不遍历用户路径。
	store.batches[id] = batch
	fail := func(cause error) (string, []string, error) { return "", nil, errors.Join(cause, store.remove(id)) }
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		path := filepath.Join(directory, file.Name)
		output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fail(ErrUnavailable)
		}
		batch.paths = append(batch.paths, path)
		store.batches[id] = batch
		count, writeError := output.Write(file.Content)
		if writeError == nil && count != len(file.Content) {
			writeError = io.ErrShortWrite
		}
		if writeError == nil {
			writeError = output.Chown(0, computer.DesktopSharedGID)
		}
		if writeError == nil {
			writeError = output.Chmod(0440)
		}
		closeError := output.Close()
		if writeError != nil || closeError != nil {
			return fail(errors.Join(ErrUnavailable, writeError, closeError))
		}
	}
	if err := os.Chown(directory, 0, computer.DesktopSharedGID); err != nil {
		return fail(ErrUnavailable)
	}
	if err := os.Chmod(directory, 0710); err != nil {
		return fail(ErrUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	batch.bytes = total
	store.batches[id] = batch
	store.bytes += total
	return id, append([]string(nil), batch.paths...), nil
}

func (store *UploadStore) remove(id string) error {
	batch, ok := store.batches[id]
	if !ok {
		return nil
	}
	var failure error
	for _, path := range batch.paths {
		failure = errors.Join(failure, os.Remove(path))
	}
	failure = errors.Join(failure, os.Remove(filepath.Join(store.directory, id)))
	if failure != nil {
		store.failure = errors.Join(store.failure, failure)
		return ErrUnavailable
	}
	store.bytes -= batch.bytes
	delete(store.batches, id)
	return nil
}

// Release 幂等清理已登记批次；缺失的实际文件视为清理失败并封闭后续上传。
func (store *UploadStore) Release(ids []string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	var failure error
	for _, id := range ids {
		failure = errors.Join(failure, store.remove(id))
	}
	return errors.Join(failure, store.failure)
}

// Close 回收所有已登记副本并保留清理错误，不能用重复关闭掩盖首次失败。
func (store *UploadStore) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return store.failure
	}
	store.closed = true
	for id := range store.batches {
		store.failure = errors.Join(store.failure, store.remove(id))
	}
	store.failure = errors.Join(store.failure, os.Remove(store.directory))
	return store.failure
}
