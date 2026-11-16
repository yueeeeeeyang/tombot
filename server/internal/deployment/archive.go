package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io.yueyang/tom/deploy"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"io.yueyang/tom/internal/dockernode"
)

func installationArchivePath(path, digest string) bool {
	return path == filepath.Join(deploy.NodeDirectory, "computer-image.tar") || len(digest) == 64 && strings.Trim(digest, "0123456789abcdef") == "" && path == filepath.Join(deploy.NodeDirectory, "computer-image-"+digest+".tar")
}

// copyInstallationArchive 将发布归档流式写入私有暂存目录，摘要不匹配时不会发布节点身份。
func (fs filesystem) copyInstallationArchive(ctx context.Context, source, destination, digest string, caller uint32) (result error) {
	if !validPath(source) || fs.directory(filepath.Dir(source), account{uid: caller}, true, false, false) != nil {
		return errors.New("离线镜像来源目录不安全")
	}
	input, err := os.OpenFile(fs.resolve(source), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("离线镜像归档无法打开")
	}
	defer func() { result = errors.Join(result, input.Close()) }()
	before, err := input.Stat()
	if err != nil {
		return errors.New("离线镜像状态无法读取")
	}
	owner, ok := infoSysOwner(before)
	if !ok || owner.Uid != fs.owner && owner.Uid != caller || owner.Nlink != 1 || !before.Mode().IsRegular() || before.Mode().Perm()&0022 != 0 || before.Size() < 1 || before.Size() > dockernode.MaximumImageArchive {
		return errors.New("离线镜像归档权限或大小无效")
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("离线镜像暂存无法创建")
	}
	defer func() { result = errors.Join(result, output.Close()) }()
	hash := sha256.New()
	reader := &installationReader{ctx: ctx, reader: input}
	if _, err := io.Copy(io.MultiWriter(output, hash), io.LimitReader(reader, dockernode.MaximumImageArchive+1)); err != nil {
		return errors.New("离线镜像复制未完成")
	}
	after, err := input.Stat()
	if err != nil || before.Size() != after.Size() || before.ModTime() != after.ModTime() || hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("离线镜像摘要不匹配或读取期间发生变化")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return errors.New("离线镜像刷盘失败")
	}
	return nil
}

type installationReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *installationReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

// verifyInstallationArchive 在重试时重新核对归档，不因已有安装记录而信任被替换的内容。
func (fs filesystem) verifyInstallationArchive(ctx context.Context, path, digest string) (result error) {
	info, err := fs.inspect(fileRule{path: path, maximum: dockernode.MaximumImageArchive, private: true}, account{})
	if err != nil {
		return err
	}
	file, err := os.OpenFile(fs.resolve(path), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("离线归档无法重新读取")
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	before, err := file.Stat()
	if err != nil || !os.SameFile(info, before) {
		return errors.New("离线归档已被替换")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(&installationReader{ctx: ctx, reader: file}, dockernode.MaximumImageArchive+1)); err != nil {
		return errors.New("离线归档复核失败")
	}
	after, err := file.Stat()
	if err != nil || before.Size() != after.Size() || before.ModTime() != after.ModTime() || hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("离线归档摘要已变化")
	}
	return nil
}
