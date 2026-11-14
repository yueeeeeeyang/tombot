package deployment

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const nodeBinaryPath = "/usr/lib/tombot/tomnode"
const nodeServicePath = "/etc/systemd/system/tombot-node.service"
const maximumNodeBinaryBytes = 256 << 20

// readInstallationSource 接受 root 或此次 sudo 调用者拥有的输入，不要求部署者先手工 chown。
// 输入仍必须位于受保护父链，不允许组写、符号链接、硬链接及读取中替换。
func (fs filesystem) readInstallationSource(path string, maximum int64, caller uint32) ([]byte, error) {
	if !validPath(path) {
		return nil, errors.New("安装输入必须是规范绝对路径")
	}
	if err := fs.directory(filepath.Dir(path), account{uid: caller}, true, false, false); err != nil {
		return nil, err
	}
	info, err := os.Lstat(fs.resolve(path))
	if err != nil {
		return nil, errors.New("程序安装输入不存在或不可读取")
	}
	owner, ok := infoSysOwner(info)
	if !ok || owner.Uid != fs.owner && owner.Uid != caller || owner.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() < 1 || info.Size() > maximum {
		return nil, errors.New("程序安装输入的归属、链接、权限或容量无效")
	}
	return fs.readInspectedFile(fileRule{path: path, maximum: maximum}, info)
}

func infoSysOwner(info os.FileInfo) (*syscall.Stat_t, bool) {
	owner, ok := info.Sys().(*syscall.Stat_t)
	return owner, ok
}

func (fs filesystem) ensureInstallDirectory(path string, mode os.FileMode) error {
	if !validPath(path) {
		return errors.New("安装目录路径无效")
	}
	current := ""
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		current += "/" + part
		if err := fs.directory(filepath.Dir(current), account{}, false, false, false); err != nil {
			return err
		}
		if err := os.Mkdir(fs.resolve(current), mode); err != nil && !errors.Is(err, os.ErrExist) {
			return errors.New("创建程序安装目录失败")
		}
		if err := fs.directory(current, account{}, false, false, false); err != nil {
			return err
		}
	}
	return nil
}

func (fs filesystem) lockNodeInstallation() (func() error, error) {
	return fs.lockInstallation("/run/tombot-node-install.lock")
}

// lockInstallation 串行化同一服务的安装、升级与状态检查，锁不随进程崩溃遗留。
func (fs filesystem) lockInstallation(path string) (func() error, error) {
	if err := fs.directory("/run", account{}, false, false, false); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(fs.resolve(path), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, errors.New("无法打开程序安装锁")
	}
	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(errors.New("无法核对程序安装锁"), file.Close())
	}
	owner, ok := infoSysOwner(info)
	if !ok || owner.Uid != fs.owner || owner.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.Join(errors.New("程序安装锁身份或权限无效"), file.Close())
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(errors.New("程序安装正在进行或无法取得独占锁"), file.Close())
	}
	return func() error { return errors.Join(syscall.Flock(int(file.Fd()), syscall.LOCK_UN), file.Close()) }, nil
}

// existingInstallationFile 仅复用内容和权限均相同的目标，避免安装器覆盖人工配置或正在运行的其他版本。
func (fs filesystem) existingInstallationFile(path string, content []byte, mode os.FileMode) (bool, error) {
	if err := fs.directory(filepath.Dir(path), account{}, false, false, true); err != nil {
		return false, err
	}
	_, err := os.Lstat(fs.resolve(path))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("无法检查程序安装目标")
	}
	rule := fileRule{path: path, maximum: maximumNodeBinaryBytes, private: mode&0077 == 0}
	info, err := fs.inspect(rule, account{})
	if err != nil {
		return false, err
	}
	if info.Mode() != mode {
		return false, errors.New("已有程序安装目标的权限不一致，未覆盖")
	}
	old, err := fs.readInspectedFile(rule, info)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(old, content) {
		return false, errors.New("已有程序或服务单元内容不同，须先排空并按升级步骤处理")
	}
	return true, nil
}

func (fs filesystem) installExactFile(ctx context.Context, path string, content []byte, mode os.FileMode) (result error) {
	if exists, err := fs.existingInstallationFile(path, content, mode); err != nil || exists {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if err := fs.ensureInstallDirectory(parent, 0755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(fs.resolve(parent), ".tombot-install-")
	if err != nil {
		return errors.New("无法创建程序暂存目录")
	}
	defer func() {
		if err := os.RemoveAll(stage); err != nil {
			result = errors.Join(result, errors.New("程序安装暂存目录清理失败"))
		}
	}()
	source := filepath.Join(stage, "content")
	if err := writeConfigurationFile(source, content, mode); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := publishPath(source, fs.resolve(path)); err != nil {
		return errors.New("程序安装目标发布失败，未覆盖已有文件")
	}
	return syncDirectory(fs.resolve(parent))
}
