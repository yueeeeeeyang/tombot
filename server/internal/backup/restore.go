package backup

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	secure "io.yueyang/tom/internal/infra/crypto"
)

type unpacker struct {
	ctx                                        context.Context
	root                                       *os.Root
	reader                                     *tar.Reader
	inventory                                  inventory
	directories                                []*tar.Header
	links                                      []string
	linkBytes                                  int
	hasData, hasWorkspace, hasDatabase, hasKey bool
}

func privateRecord(header *tar.Header, name string, limit int64) bool {
	return header.Name == name && header.Typeflag == tar.TypeReg && header.Size > 0 && header.Size <= limit
}

// Restore 在目标同级的私有暂存目录内验证整份归档，以一次改名同时发布数据与工作区。
func Restore(ctx context.Context, archive, target, environment string) (result error) {
	dest, err := outputPath(target)
	if err != nil {
		return err
	}
	source, err := openArchive(archive)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, source.Close()) }()
	raw := bufio.NewReader(cancelReader{ctx, source})
	compressed, err := gzip.NewReader(raw)
	if err != nil {
		return fmt.Errorf("备份压缩格式无效：%w", err)
	}
	defer func() { result = errors.Join(result, compressed.Close()) }()
	compressed.Multistream(false)
	stage, err := os.MkdirTemp(filepath.Dir(dest), ".tom-restore-*")
	if err != nil {
		return err
	}
	defer func() {
		if stage != "" {
			result = errors.Join(result, removeStage(stage))
		}
	}()
	r, err := os.OpenRoot(stage)
	if err != nil {
		return err
	}
	u := unpacker{ctx: ctx, root: r, reader: tar.NewReader(compressed), inventory: inventory{digest: sha256.New()}}
	err = errors.Join(u.unpack(environment), r.Close())
	if err != nil {
		return err
	}
	// tar 在结束块就停止，继续读到 gzip 校验尾才算整份归档有效。
	var trailing [1]byte
	if n, err := io.ReadFull(compressed, trailing[:]); n != 0 || err != io.EOF {
		return fmt.Errorf("备份压缩尾部损坏或含附加内容")
	}
	if _, err := raw.Peek(1); err != io.EOF {
		return fmt.Errorf("备份包含额外压缩成员或尾随内容")
	}
	if err = syncDir(stage); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = publish(stage, dest); err != nil {
		return err
	}
	stage = ""
	if err = syncDir(filepath.Dir(dest)); err != nil {
		return fmt.Errorf("还原目录已发布，但目录同步失败：%w", err)
	}
	return nil
}

func (u *unpacker) unpack(environment string) error {
	header, err := u.reader.Next()
	if err != nil {
		return err
	}
	if !privateRecord(header, infoName, 4096) {
		return fmt.Errorf("缺少有效备份说明")
	}
	if err = u.inventory.include(header); err != nil {
		return err
	}
	body, err := io.ReadAll(io.TeeReader(u.reader, u.inventory.digest))
	if err != nil {
		return err
	}
	var info metadata
	if err = json.Unmarshal(body, &info); err != nil {
		return fmt.Errorf("备份说明损坏")
	}
	if info.Version != 1 || len(info.Salt) != 32 || len(info.KeyCheck) != sha256.Size {
		return fmt.Errorf("备份版本或密钥校验信息无效")
	}
	var key []byte
	defer func() { clear(key) }()
	switch info.KeySource {
	case "environment":
		if environment == "" {
			return fmt.Errorf("还原此备份需要原来的 TOM_MASTER_KEY")
		}
		key, err = secure.ReadKey("", environment)
		if err != nil {
			return err
		}
		if !hmac.Equal(info.KeyCheck, authenticate(key, "tom.backup.key.v1", info.Salt)) {
			return fmt.Errorf("TOM_MASTER_KEY 与备份不匹配")
		}
	case "file":
	default:
		return fmt.Errorf("备份声明了未知的密钥来源")
	}
	for {
		if err = u.ctx.Err(); err != nil {
			return err
		}
		header, err = u.reader.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("归档缺少完整性校验记录")
		}
		if err != nil {
			return err
		}
		if header.Name == proofName {
			break
		}
		if err = u.extract(header, info.KeySource); err != nil {
			return err
		}
	}
	if !privateRecord(header, proofName, sha256.Size) || header.Size != sha256.Size {
		return fmt.Errorf("归档校验记录无效")
	}
	proof, err := io.ReadAll(u.reader)
	if err != nil {
		return err
	}
	if _, err = u.reader.Next(); err != io.EOF {
		return fmt.Errorf("校验记录之后仍有归档成员或数据损坏")
	}
	if !u.hasData || !u.hasWorkspace || !u.hasDatabase || u.hasKey != (info.KeySource == "file") {
		return fmt.Errorf("归档缺少必要的数据、工作区或主密钥")
	}
	dataDir := filepath.Join(u.root.Name(), "data")
	if info.KeySource == "file" {
		key, err = secure.ReadKey(dataDir, "")
		if err != nil {
			return err
		}
		if !hmac.Equal(info.KeyCheck, authenticate(key, "tom.backup.key.v1", info.Salt)) {
			return fmt.Errorf("归档中的主密钥校验失败")
		}
		if environment != "" {
			supplied, err := secure.ReadKey("", environment)
			if err != nil {
				return err
			}
			same := hmac.Equal(key, supplied)
			clear(supplied)
			if !same {
				return fmt.Errorf("当前 TOM_MASTER_KEY 会覆盖归档中的文件主密钥，请使用相同密钥或取消环境配置")
			}
		}
	}
	if !hmac.Equal(proof, authenticate(key, "tom.backup.content.v1", u.inventory.digest.Sum(nil))) {
		return fmt.Errorf("归档内容或元数据完整性校验失败")
	}
	if err = u.leaveDirectories(""); err != nil {
		return err
	}
	for _, top := range []string{"data", "workspace"} {
		r, err := u.root.OpenRoot(top)
		if err != nil {
			return err
		}
		err = errors.Join(u.verifyLinks(r, top), r.Close())
		if err != nil {
			return err
		}
	}
	return verifyDatabase(u.ctx, filepath.Join(dataDir, "tom.db"), dataDir, key)
}

func (u *unpacker) verifyLinks(r *os.Root, top string) error {
	for _, name := range u.links {
		tree, relative, _ := strings.Cut(name, "/")
		if tree != top {
			continue
		}
		if err := u.ctx.Err(); err != nil {
			return err
		}
		if _, err := r.Stat(relative); err != nil {
			return fmt.Errorf("还原链接无法在同一目录树内解析：%w", err)
		}
	}
	return nil
}

func (u *unpacker) extract(h *tar.Header, keySource string) error {
	name := h.Name
	if err := checkedName(name); err != nil {
		return err
	}
	top, _, nested := strings.Cut(name, "/")
	if top != "data" && top != "workspace" || operational(name) || !legalControlName(name) || h.Mode < 0 || h.Mode > 0777 {
		return fmt.Errorf("归档包含保留名称或不允许的权限")
	}
	if !nested && h.Typeflag != tar.TypeDir {
		return fmt.Errorf("归档根必须是目录")
	}
	if name == "data/tom.db" || name == "data/auth.json" || strings.EqualFold(name, "data/master.key") {
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("数据库、认证和主密钥必须为普通文件")
		}
		if name == "data/auth.json" && h.Size > 4096 {
			return fmt.Errorf("认证文件超出容量")
		}
		if strings.EqualFold(name, "data/master.key") && (name != "data/master.key" || keySource != "file" || h.Size != 32 || h.Mode&0077 != 0) {
			return fmt.Errorf("归档主密钥与来源、大小或权限不符")
		}
	}
	if err := u.inventory.include(h); err != nil {
		return err
	}
	// 只接受深度优先目录成员。每个父目录都必须实际由本次解包创建，链接不能成为写入父目录。
	parent := path.Dir(name)
	if err := u.leaveDirectories(parent); err != nil {
		return err
	}
	if parent != "." && (len(u.directories) == 0 || u.directories[len(u.directories)-1].Name != parent) {
		return fmt.Errorf("归档父目录缺失或成员顺序无效")
	}
	switch h.Typeflag {
	case tar.TypeDir:
		if h.Size != 0 {
			return fmt.Errorf("目录长度无效")
		}
		if err := u.root.Mkdir(name, 0700); err != nil {
			return err
		}
		u.directories = append(u.directories, h)
		u.hasData = u.hasData || name == "data"
		u.hasWorkspace = u.hasWorkspace || name == "workspace"
	case tar.TypeReg:
		if err := u.file(h); err != nil {
			return err
		}
		u.hasKey = u.hasKey || name == "data/master.key"
		u.hasDatabase = u.hasDatabase || name == "data/tom.db"
	case tar.TypeSymlink:
		if err := relativeLink(name, h.Linkname); err != nil {
			return err
		}
		u.linkBytes += len(name)
		if h.Size != 0 || u.linkBytes > 32<<20 {
			return fmt.Errorf("归档链接长度或总量超限")
		}
		if err := u.root.Symlink(h.Linkname, name); err != nil {
			return err
		}
		u.links = append(u.links, name)
	default:
		return fmt.Errorf("归档包含不支持的文件类型")
	}
	return nil
}

func (u *unpacker) file(h *tar.Header) (result error) {
	file, err := u.root.OpenFile(h.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	if _, err = io.CopyN(io.MultiWriter(file, u.inventory.digest), cancelReader{u.ctx, u.reader}, h.Size); err != nil {
		return err
	}
	if err = file.Chmod(os.FileMode(h.Mode)); err != nil {
		return err
	}
	if err = u.root.Chtimes(h.Name, time.Time{}, h.ModTime); err != nil {
		return err
	}
	return file.Sync()
}

// leaveDirectories 在所有子项完成后才恢复目录时间与权限，最多持有树深度个元数据记录。
func (u *unpacker) leaveDirectories(parent string) error {
	for len(u.directories) > 0 {
		last := len(u.directories) - 1
		h := u.directories[last]
		if h.Name == parent {
			break
		}
		if err := u.root.Chtimes(h.Name, time.Time{}, h.ModTime); err != nil {
			return err
		}
		if err := u.root.Chmod(h.Name, os.FileMode(h.Mode)); err != nil {
			return err
		}
		if err := syncDir(filepath.Join(u.root.Name(), h.Name)); err != nil {
			return err
		}
		u.directories = u.directories[:last]
	}
	return nil
}

// removeStage 先恢复暂存目录的所有者访问权限，否则损坏归档中的只读目录可能阻碍清理。WalkDir 不跟随链接。
func removeStage(stage string) error {
	if _, err := os.Lstat(stage); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	err := filepath.WalkDir(stage, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(name, 0700)
		}
		return nil
	})
	return errors.Join(err, os.RemoveAll(stage))
}
