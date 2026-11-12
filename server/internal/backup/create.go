package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/store"
)

type packer struct {
	ctx            context.Context
	writer         *tar.Writer
	inventory      inventory
	database       string
	environmentKey bool
	key            []byte
	linkBytes      int
	excludedKey    fs.FileInfo
}

// Create 取得服务同一把排他锁，制作 SQLite 快照并归档两棵目录树。完整成功后才发布最终文件。
func Create(ctx context.Context, dataDir, workspaceDir, output, environment string) (result error) {
	dataDir, err := resolvedPath(dataDir)
	if err != nil {
		return err
	}
	workspaceDir, err = resolvedPath(workspaceDir)
	if err != nil {
		return err
	}
	dest, err := outputPath(output)
	if err != nil {
		return err
	}
	for _, pair := range [][2]string{{dataDir, workspaceDir}, {workspaceDir, dataDir}} {
		source, err := os.Stat(pair[0])
		if err != nil {
			return err
		}
		if !source.IsDir() {
			return fmt.Errorf("数据根和工作区根必须为目录")
		}
		if err := checkParents(pair[1], []fs.FileInfo{source}); err != nil {
			return err
		}
	}
	if err = CheckDestination(dest, dataDir, workspaceDir); err != nil {
		return err
	}
	lock, err := store.LockDirectory(dataDir)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, lock.Close()) }()
	for _, name := range []string{"tom.db", "tom.db-wal", "tom.db-shm", "tom.db-journal", "auth.json"} {
		stat, err := os.Lstat(filepath.Join(dataDir, name))
		if name != "tom.db" && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !stat.Mode().IsRegular() || name == "auth.json" && stat.Size() > 4096 {
			return fmt.Errorf("数据库或认证文件类型、大小无效")
		}
	}
	key, err := secure.ReadKey(dataDir, environment)
	if err != nil {
		return err
	}
	defer clear(key)
	stage, err := os.MkdirTemp(filepath.Dir(dest), ".tom-backup-*")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, os.RemoveAll(stage)) }()
	filename := filepath.Join(stage, "tom.db")
	if err = snapshot(ctx, filepath.Join(dataDir, "tom.db"), filename); err != nil {
		return err
	}
	if err = verifyDatabase(ctx, filename, dataDir, key); err != nil {
		return err
	}
	temporary := filepath.Join(stage, "archive")
	f, err := os.OpenFile(temporary, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	compressed, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		return errors.Join(err, f.Close())
	}
	p := packer{ctx: ctx, writer: tar.NewWriter(compressed), inventory: inventory{digest: sha256.New()}, database: filename, key: key, environmentKey: environment != ""}
	if p.environmentKey {
		// 环境模式也拒绝工作区内指向旧文件密钥的硬链接，避免通过别名将密钥打包。
		p.excludedKey, err = os.Stat(filepath.Join(dataDir, "master.key"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return errors.Join(err, p.writer.Close(), compressed.Close(), f.Close())
		}
	}
	writeErr := p.write(dataDir, workspaceDir, key)
	if err = errors.Join(writeErr, p.writer.Close(), compressed.Close(), f.Sync(), f.Close()); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = publish(temporary, dest); err != nil {
		return err
	}
	if err = syncDir(filepath.Dir(dest)); err != nil {
		return fmt.Errorf("归档已发布，但目录同步失败：%w", err)
	}
	return nil
}

func (p *packer) write(dataDir, workspaceDir string, key []byte) error {
	salt, err := secure.Random(32)
	if err != nil {
		return err
	}
	description := metadata{Version: 1, KeySource: "file", Notice: fileNotice, Salt: salt, KeyCheck: authenticate(key, "tom.backup.key.v1", salt)}
	if p.environmentKey {
		description.KeySource = "environment"
		description.Notice = envNotice
	}
	body, err := json.Marshal(description)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if err = p.entry(newHeader(infoName, tar.TypeReg, 0600, int64(len(body)), now), bytes.NewReader(body)); err != nil {
		return err
	}
	for _, tree := range []struct{ name, root string }{{"data", dataDir}, {"workspace", workspaceDir}} {
		r, err := os.OpenRoot(tree.root)
		if err != nil {
			return err
		}
		err = errors.Join(p.walk(r, tree.name, "."), r.Close())
		if err != nil {
			return err
		}
	}
	proof := authenticate(key, "tom.backup.content.v1", p.inventory.digest.Sum(nil))
	if err = p.writer.WriteHeader(newHeader(proofName, tar.TypeReg, 0600, int64(len(proof)), now)); err != nil {
		return err
	}
	_, err = p.writer.Write(proof)
	return err
}

func (p *packer) entry(header *tar.Header, source io.Reader) error {
	if err := p.inventory.include(header); err != nil {
		return err
	}
	if err := p.writer.WriteHeader(header); err != nil {
		return err
	}
	if header.Size > 0 {
		_, err := io.CopyN(io.MultiWriter(p.writer, p.inventory.digest), cancelReader{p.ctx, source}, header.Size)
		return err
	}
	return nil
}

// walk 使用目录句柄且分批枚举，显式拒绝特殊文件。打开普通文件后再核对 inode 与修改状态。
func (p *packer) walk(root *os.Root, top, relative string) (result error) {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	name := filepath.ToSlash(filepath.Join(top, relative))
	if err := checkedName(name); err != nil {
		return err
	}
	if !legalControlName(name) {
		return fmt.Errorf("数据控制文件名称大小写不规范")
	}
	if operational(name) || name == "data/master.key" && p.environmentKey {
		return nil
	}
	stat, err := root.Lstat(relative)
	if err != nil {
		return err
	}
	switch {
	case stat.IsDir():
		if err = p.entry(newHeader(name, tar.TypeDir, stat.Mode(), 0, stat.ModTime()), nil); err != nil {
			return err
		}
		dir, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, dir.Close()) }()
		for {
			entries, err := dir.ReadDir(128)
			for _, child := range entries {
				if err := p.walk(root, top, filepath.Join(relative, child.Name())); err != nil {
					return err
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
		}
		after, err := dir.Stat()
		if err != nil {
			return err
		}
		if !os.SameFile(stat, after) || !stat.ModTime().Equal(after.ModTime()) {
			return fmt.Errorf("备份时目录发生变化，请停止写入后重试")
		}
		return nil
	case stat.Mode()&os.ModeSymlink != 0:
		p.linkBytes += len(name)
		if p.linkBytes > 32<<20 {
			return fmt.Errorf("归档链接总量超限")
		}
		target, err := root.Readlink(relative)
		if err != nil {
			return err
		}
		if err = relativeLink(name, target); err != nil {
			return err
		}
		if err = p.verifyLink(root, top, relative); err != nil {
			return fmt.Errorf("归档链接无法在原目录树内解析：%w", err)
		}
		header := newHeader(name, tar.TypeSymlink, stat.Mode(), 0, stat.ModTime())
		header.Linkname = target
		return p.entry(header, nil)
	case stat.Mode().IsRegular():
		if p.excludedKey != nil && os.SameFile(stat, p.excludedKey) {
			return fmt.Errorf("归档其他路径引用了环境模式下应排除的文件密钥")
		}
		if name == "data/master.key" {
			return p.entry(newHeader(name, tar.TypeReg, 0600, int64(len(p.key)), stat.ModTime()), bytes.NewReader(p.key))
		}
		var file *os.File
		if name == "data/tom.db" {
			file, err = os.Open(p.database)
		} else {
			file, err = root.OpenFile(relative, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		}
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, file.Close()) }()
		before, err := file.Stat()
		if err != nil {
			return err
		}
		if !before.Mode().IsRegular() || name != "data/tom.db" && !os.SameFile(stat, before) {
			return fmt.Errorf("备份时文件已被替换")
		}
		if err = p.entry(newHeader(name, tar.TypeReg, stat.Mode(), before.Size(), stat.ModTime()), file); err != nil {
			return err
		}
		after, err := file.Stat()
		if err != nil {
			return err
		}
		if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return fmt.Errorf("备份时文件发生变化，请停止写入后重试")
		}
		return nil
	default:
		return fmt.Errorf("归档包含不支持的特殊文件，请停止相关进程并整理后重试")
	}
}

// verifyLink 核对链接链的每一跳。即使最终目标可读，中间依赖被排除的临时文件也会产生不可还原归档。
func (p *packer) verifyLink(root *os.Root, top, relative string) error {
	if _, err := root.Stat(relative); err != nil {
		return err
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	current := "."
	hops := 0
	for len(parts) > 0 {
		part := parts[0]
		parts = parts[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			if current == "." {
				return fmt.Errorf("链接越过归档根目录")
			}
			current = filepath.Dir(current)
			continue
		}
		current = filepath.Join(current, part)
		name := filepath.ToSlash(filepath.Join(top, current))
		if operational(name) || (p.environmentKey && name == "data/master.key") {
			return fmt.Errorf("链接依赖归档排除的文件")
		}
		stat, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if stat.Mode()&os.ModeSymlink != 0 {
			hops++
			if hops > 40 {
				return fmt.Errorf("链接链过长")
			}
			target, err := root.Readlink(current)
			if err != nil {
				return err
			}
			if err = relativeLink(name, target); err != nil {
				return err
			}
			parts = append(strings.Split(target, "/"), parts...)
			current = filepath.Dir(current)
		}
	}
	return nil
}
