package dockernode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"io.yueyang/tom/internal/computer"
)

// FileStorageOptions 由受保护节点配置提供；镜像目录与控制记录目录必须分离。
type FileStorageOptions struct {
	Directory        string
	StateDirectory   string
	CommandTimeout   time.Duration
	MinimumFreeBytes uint64
}

// Validate 只校验配置值和目录边界；真实所有者、工具和内核状态由构造器及每次操作验证。
func (options FileStorageOptions) Validate() error {
	if options.CommandTimeout < time.Second || options.CommandTimeout > 5*time.Minute || options.MinimumFreeBytes < 1<<30 || options.MinimumFreeBytes > 100<<30 {
		return errors.New("持久磁盘时限或磁盘余量配置无效")
	}
	for _, directory := range []string{options.Directory, options.StateDirectory} {
		if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || directory == "/" {
			return errors.New("持久磁盘目录无效")
		}
	}
	if options.Directory == options.StateDirectory || strings.HasPrefix(options.StateDirectory, options.Directory+string(filepath.Separator)) {
		return errors.New("持久磁盘控制记录必须与用户数据分离")
	}
	return nil
}

// FileStorage 为每次初始供给创建固定容量 ext4 文件系统，宿主预分配空间，容器只访问挂载结果。
// 生命周期由持久记录驱动；重启可重新挂载已有文件系统，绝不重新格式化已存在的用户数据。
type FileStorage struct {
	options        FileStorageOptions
	journal        *journal
	owner          uint32
	layout         diskLayoutIdentity
	run            storageCommand
	mounts         func() ([]filesystemMount, error)
	freeBytes      func(string) (uint64, error)
	filesystemSize func(string) (int64, error)
	pinLoop        func(loopDevice, *storageRecord) (func() error, error)
}

// NewFileStorage 检查 Linux root 权限、受保护目录及固定宿主工具；缺失依赖时拒绝启动。
func NewFileStorage(options FileStorageOptions) (*FileStorage, error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return nil, errors.New("持久磁盘后端要求 Linux 受信系统服务")
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	for _, path := range []string{options.Directory, options.StateDirectory} {
		if err := checkProtectedParents(path); err != nil {
			return nil, err
		}
	}
	commands, err := storageCommands(options.CommandTimeout)
	if err != nil {
		return nil, err
	}
	journal, err := openJournal(options.StateDirectory, 0)
	if err != nil {
		return nil, err
	}
	storage := &FileStorage{options: options, journal: journal, owner: 0, run: commands, mounts: readMounts, freeBytes: availableStorageBytes, filesystemSize: mountedFilesystemSize, pinLoop: pinLoopDevice}
	storage.layout = diskLayoutIdentity{rootUID: 0, rootGID: 0, sharedGID: computer.DesktopSharedGID, browserUID: computer.BrowserUID}
	if err := storage.validateStorageLayout(); err != nil {
		return nil, errors.Join(err, storage.Close())
	}
	return storage, nil
}

// Close 仅关闭记录目录，不卸载仍在使用的用户磁盘；服务正常重启不影响已挂载数据。
func (storage *FileStorage) Close() error { return storage.journal.root.Close() }

// VerifyFilesystem 将容器探针读到的容量与当前已核验 ext4 挂载相对照，不用规格容量替代实际容量。
func (storage *FileStorage) VerifyFilesystem(ctx context.Context, resource Resource, filesystemType int64, size uint64) error {
	_, err := storage.locked(ctx, resource, func(ctx context.Context, resource Resource) (Workspace, error) {
		return storage.verifyFilesystem(ctx, resource, filesystemType, size)
	})
	return err
}

func (storage *FileStorage) verifyFilesystem(ctx context.Context, resource Resource, filesystemType int64, size uint64) (Workspace, error) {
	const ext4FilesystemType int64 = 0xef53
	if filesystemType != ext4FilesystemType || size == 0 {
		return Workspace{}, failure("storage_probe_filesystem_invalid")
	}
	workspace, err := storage.inspect(ctx, resource)
	if err != nil {
		return workspace, err
	}
	if !workspace.Exists || !workspace.Ready {
		return workspace, failure("storage_probe_mount_missing")
	}
	actual, err := storage.filesystemSize(resource.WorkspacePath)
	if err != nil {
		return workspace, err
	}
	if actual <= 0 || uint64(actual) != size {
		return workspace, failure("storage_probe_capacity_mismatch")
	}
	return workspace, ctx.Err()
}

func (storage *FileStorage) validate(resource Resource) error {
	if resource.Specification.Validate() != nil || resource.Name != resourceName(resource.Specification) || resource.WorkspacePath != filepath.Join(storage.options.Directory, resource.Name, "workspace") {
		return failure("storage_identity_invalid")
	}
	return nil
}

func (storage *FileStorage) directory(resource Resource) string {
	return filepath.Join(storage.options.Directory, resource.Name)
}
func (storage *FileStorage) image(resource Resource) string {
	return filepath.Join(storage.directory(resource), "filesystem.img")
}

func filesystemUUID(resource Resource) string {
	digest := sha256.Sum256([]byte("tombot.workspace\x00" + resource.Specification.NodeID + "\x00" + resource.Specification.UserID + "\x00" + resource.Name))
	value := hex.EncodeToString(digest[:16])
	return value[:8] + "-" + value[8:12] + "-" + value[12:16] + "-" + value[16:20] + "-" + value[20:]
}

func (storage *FileStorage) locked(ctx context.Context, resource Resource, apply func(context.Context, Resource) (Workspace, error)) (workspace Workspace, result error) {
	if err := storage.validate(resource); err != nil {
		return workspace, err
	}
	release, err := storage.journal.lock(ctx, resource.Specification.UserID+".storage")
	if err != nil {
		return workspace, err
	}
	defer func() { result = errors.Join(result, release()) }()
	if err = ctx.Err(); err != nil {
		return workspace, err
	}
	return apply(ctx, resource)
}

// Inspect 返回持久资源是否存在及是否已挂载，异常和未知状态不能被解释为空磁盘。
func (storage *FileStorage) Inspect(ctx context.Context, resource Resource) (Workspace, error) {
	return storage.locked(ctx, resource, storage.inspect)
}

// Ensure 仅对首次分配的新镜像格式化；持久格式化意图后的重试只做只读核验，不能再次覆盖镜像。
func (storage *FileStorage) Ensure(ctx context.Context, resource Resource) (Workspace, error) {
	return storage.locked(ctx, resource, storage.ensure)
}

// Remove 仅在节点已确认本次未启用容器及卷被删除后调用；忙挂载和延迟解除关联都阻止删除镜像。
func (storage *FileStorage) Remove(ctx context.Context, resource Resource) error {
	_, err := storage.locked(ctx, resource, storage.remove)
	return err
}

func (storage *FileStorage) workspace(resource Resource, ready bool) Workspace {
	return Workspace{Exists: true, Ready: ready, Path: resource.WorkspacePath, CapacityBytes: resource.Specification.Resources.DiskBytes}
}

func (storage *FileStorage) inspect(ctx context.Context, resource Resource) (Workspace, error) {
	state, err := storage.load(resource)
	if err != nil {
		return Workspace{}, err
	}
	if state == nil || state.Phase == "removed" {
		if err = storage.requireAbsentDirectory(resource); err != nil {
			return Workspace{}, err
		}
		return Workspace{}, nil
	}
	if err = storage.checkDirectory(resource, false); err != nil {
		return Workspace{}, err
	}
	mounted, err := storage.mounted(ctx, resource, state)
	if err != nil {
		return Workspace{}, err
	}
	if mounted && state.Phase != "formatted" && state.Phase != "ready" && state.Phase != "removing" {
		return Workspace{}, failure("storage_mount_phase_mismatch")
	}
	ready := mounted && state.Phase == "ready"
	if ready {
		if err = storage.layout.prepare(resource.WorkspacePath, false); err != nil {
			return Workspace{}, err
		}
	}
	return storage.workspace(resource, ready), nil
}

func (storage *FileStorage) requireAbsentDirectory(resource Resource) error {
	_, err := os.Lstat(storage.directory(resource))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("读取用户磁盘目录失败")
	}
	return failure("storage_unowned_directory")
}

func (storage *FileStorage) checkDirectory(resource Resource, create bool) error {
	path := storage.directory(resource)
	if create {
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return errors.New("创建用户磁盘目录失败")
		}
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) && !create {
		return nil
	}
	if err := checkProtectedDirectory(path, storage.owner); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return errors.New("读取用户磁盘目录失败")
	}
	for _, entry := range entries {
		if entry.Name() != "workspace" && entry.Name() != "filesystem.img" {
			return failure("storage_directory_has_foreign_files")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return failure("storage_directory_has_link")
		}
		if entry.Name() == "workspace" && !entry.IsDir() {
			return failure("storage_mountpoint_type_invalid")
		}
	}
	return nil
}

func (storage *FileStorage) command(ctx context.Context, program string, arguments ...string) ([]byte, error) {
	result, err := storage.run(ctx, program, arguments...)
	if err != nil {
		return nil, err
	}
	if result.exitCode != 0 {
		return nil, fmt.Errorf("持久磁盘操作 %s 失败（退出码 %d）", program, result.exitCode)
	}
	return result.output, nil
}
