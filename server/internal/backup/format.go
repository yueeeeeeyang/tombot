// Package backup 将停机实例的数据与工作区作为一份可验证的归档交付。
package backup

import (
	"archive/tar"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	infoName              = "tom-backup.json"
	proofName             = "tom-backup.hmac"
	fileNotice            = "此归档含解密密钥，需自行加密保存"
	envNotice             = "还原时需提供原来的 TOM_MASTER_KEY"
	maxArchiveBytes int64 = 256 << 30
	maxEntries            = 1_000_000
	maxDepth              = 128
)

// KeyNotice 返回与主密钥来源对应的归档保管说明。
func KeyNotice(environment string) string {
	if environment != "" {
		return envNotice
	}
	return fileNotice
}

type metadata struct {
	Version   int    `json:"version"`
	KeySource string `json:"key_source"`
	Notice    string `json:"notice"`
	Salt      []byte `json:"salt"`
	KeyCheck  []byte `json:"key_check"`
}

func authenticate(key []byte, label string, value []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(label))
	mac.Write(value)
	return mac.Sum(nil)
}

type inventory struct {
	digest  hash.Hash
	entries int
	bytes   int64
}

// include 把文件名、类型、权限、时间、长度与链接目标纳入校验，防止有效内容被换位。
func (inventory *inventory) include(header *tar.Header) error {
	if inventory.entries >= maxEntries || header.Size < 0 || header.Size > maxArchiveBytes-inventory.bytes {
		return fmt.Errorf("归档文件数量或展开大小超限")
	}
	inventory.entries++
	inventory.bytes += header.Size
	record := []any{header.Name, header.Typeflag, header.Mode, header.ModTime.UnixNano(), header.Size, header.Linkname}
	return json.NewEncoder(inventory.digest).Encode(record)
}

type cancelReader struct {
	ctx    context.Context
	source io.Reader
}

func (r cancelReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(buffer)
}

func checkedName(name string) error {
	if name == "." || !fs.ValidPath(name) || len(name) > 4096 || strings.ContainsAny(name, "\\\x00") || strings.Count(name, "/") >= maxDepth {
		return fmt.Errorf("归档中存在无效路径")
	}
	return nil
}

// relativeLink 只接受留在同一个逻辑目录树内的相对链接，实际链接链在根句柄内再次解析。
func relativeLink(name, target string) error {
	if target == "" || len(target) > 4096 || strings.ContainsAny(target, "\\\x00") || path.IsAbs(target) {
		return fmt.Errorf("归档包含不可移植的链接")
	}
	resolved := path.Clean(path.Join(path.Dir(name), target))
	top, _, _ := strings.Cut(name, "/")
	if resolved != top && !strings.HasPrefix(resolved, top+"/") {
		return fmt.Errorf("归档链接越过数据或工作区边界")
	}
	return nil
}

func operational(name string) bool {
	switch strings.ToLower(name) {
	case "data/.tom.lock", "data/tom.db-wal", "data/tom.db-shm", "data/tom.db-journal",
		"data/browser-profile/singletonlock", "data/browser-profile/singletoncookie", "data/browser-profile/singletonsocket", "data/browser-profile/devtoolsactiveport", "data/browser-profile/runningchromeversion":
		return true
	}
	return false
}

func newHeader(name string, kind byte, mode os.FileMode, size int64, modified time.Time) *tar.Header {
	return &tar.Header{Name: name, Typeflag: kind, Mode: int64(mode.Perm()), Size: size, ModTime: modified, Format: tar.FormatPAX}
}

func syncDir(directory string) error {
	f, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func resolvedPath(name string) (string, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

// legalControlName 保留核心文件的唯一拼写，避免跨大小写敏感文件系统还原时产生别名。
func legalControlName(name string) bool {
	switch strings.ToLower(name) {
	case "data/tom.db", "data/master.key", "data/auth.json":
		return name == strings.ToLower(name)
	}
	return true
}

// outputPath 规范父目录但不解析最终文件。已有目标和符号链接均拒绝覆盖。
func outputPath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("需要指定输出位置")
	}
	absolute, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	dest := filepath.Join(parent, filepath.Base(absolute))
	if _, err := os.Lstat(dest); err == nil {
		return "", fmt.Errorf("目标已存在，请指定新的路径")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return dest, nil
}

// CheckDestination 在创建暂存文件前沿真实父目录核对 inode，也覆盖大小写别名和指向源目录的链接。
func CheckDestination(name string, sources ...string) error {
	dest, err := outputPath(name)
	if err != nil {
		return err
	}
	var protected []fs.FileInfo
	for _, source := range sources {
		stat, err := os.Stat(source)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		protected = append(protected, stat)
	}
	return checkParents(filepath.Dir(dest), protected)
}

func checkParents(directory string, protected []fs.FileInfo) error {
	for {
		stat, err := os.Stat(directory)
		if err != nil {
			return err
		}
		for _, other := range protected {
			if os.SameFile(stat, other) {
				return fmt.Errorf("数据、工作区与输出位置必须分离")
			}
		}
		parent := filepath.Dir(directory)
		if directory == parent {
			return nil
		}
		directory = parent
	}
}

func openArchive(name string) (*os.File, error) {
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	stat, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !stat.Mode().IsRegular() {
		return nil, errors.Join(fmt.Errorf("归档必须是普通文件"), file.Close())
	}
	return file, nil
}
