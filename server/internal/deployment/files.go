package deployment

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type filesystem struct {
	base  string
	owner uint32
}

type fileRule struct {
	path    string
	maximum int64
	exact   int64
	private bool
	api     bool
}

func (value configuration) references() []fileRule {
	server := value.server
	files := []fileRule{
		{path: server.TLSCertificateFile, maximum: 1 << 20, api: true},
		{path: server.TLSPrivateKeyFile, maximum: 64 << 10, private: true, api: true},
		{path: server.IdentityKeyFile, maximum: 32, exact: 32, private: true, api: true},
		{path: server.CSRFKeyFile, maximum: 32, exact: 32, private: true, api: true},
	}
	for _, path := range server.ModelKeyFiles {
		files = append(files, fileRule{path: path, maximum: 32, exact: 32, private: true, api: true})
	}
	for _, path := range server.Database.DSNFiles {
		files = append(files, fileRule{path: path, maximum: 16 << 10, private: true, api: true})
	}
	if server.Models.RootCAFile != "" {
		files = append(files, fileRule{path: server.Models.RootCAFile, maximum: 1 << 20, api: true})
	}
	// 节点控制证书由 API 持有、节点证书与私钥由节点持有；跨主机握手依赖它们，必须先核对身份与权限。
	files = append(files,
		fileRule{path: server.Node.CertificateFile, maximum: 1 << 20, api: true},
		fileRule{path: server.Node.PrivateKeyFile, maximum: 64 << 10, private: true, api: true},
		fileRule{path: value.node.Control.CertificateFile, maximum: 1 << 20},
		fileRule{path: value.node.Control.PrivateKeyFile, maximum: 64 << 10, private: true},
	)
	if value.image.AuthenticationFile != "" {
		files = append(files, fileRule{path: value.image.AuthenticationFile, maximum: 16 << 10, private: true})
	}
	return files
}

func (value configuration) referencePaths() []string {
	var paths []string
	for _, file := range value.references() {
		paths = append(paths, file.path)
	}
	return paths
}

func (fs filesystem) resolve(path string) string {
	return filepath.Join(fs.base, strings.TrimPrefix(path, "/"))
}

func accessible(info os.FileInfo, api account, mask os.FileMode) bool {
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	permissions := info.Mode().Perm()
	if owner.Uid == api.uid {
		return permissions&(mask<<6) == mask<<6
	}
	if owner.Gid == api.gid {
		return permissions&(mask<<3) == mask<<3
	}
	return permissions&mask == mask
}

// directory 检查整个父链，避免 root 检查通过但 API 无法遍历，或通过目录链接进入其他位置。
// planned 只允许尚未创建的状态目录；其创建权限仍由实际组件和 systemd 验证。
func (fs filesystem) directory(path string, api account, allowAPI, requireAccess, planned bool) error {
	if path != "/" && !validPath(path) {
		return errors.New("部署目录路径无效")
	}
	current := fs.base
	parts := []string{""}
	if path != "/" {
		parts = append(parts, strings.Split(strings.TrimPrefix(path, "/"), "/")...)
	}
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if planned && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return errors.New("部署父目录不可读取")
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode().Perm()&0022 != 0 || owner.Uid != fs.owner && (!allowAPI || owner.Uid != api.uid) {
			return errors.New("部署父目录存在链接或身份权限不安全")
		}
		if requireAccess && !accessible(info, api, 1) {
			return errors.New("API 系统账户无法遍历部署目录")
		}
	}
	return nil
}

func (fs filesystem) inspect(rule fileRule, api account) (os.FileInfo, error) {
	if !validPath(rule.path) {
		return nil, errors.New("部署文件路径无效")
	}
	if err := fs.directory(filepath.Dir(rule.path), api, rule.api, rule.api, false); err != nil {
		return nil, err
	}
	info, err := os.Lstat(fs.resolve(rule.path))
	if err != nil {
		return nil, errors.New("部署文件不存在或不可读取")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeType != 0 || owner.Nlink != 1 || info.Mode().Perm()&0022 != 0 || info.Size() < 1 || info.Size() > rule.maximum || rule.exact != 0 && info.Size() != rule.exact {
		return nil, errors.New("部署文件类型、链接数、权限或容量无效")
	}
	if rule.private && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("私有部署文件权限过宽")
	}
	if rule.api {
		if owner.Uid != fs.owner && owner.Uid != api.uid || !accessible(info, api, 4) {
			return nil, errors.New("API 文件归属不正确或不可读取")
		}
	} else if owner.Uid != fs.owner {
		return nil, errors.New("部署配置必须属于 root")
	}
	return info, nil
}

// readConfig 只打开非秘密配置。凭据检查停留在 inspect，不读取或输出内容。
func (fs filesystem) readConfig(rule fileRule, api account) (encoded []byte, result error) {
	if err := fs.directory(filepath.Dir(rule.path), api, false, rule.api, false); err != nil {
		return nil, err
	}
	before, err := fs.inspect(rule, api)
	if err != nil {
		return nil, err
	}
	owner := before.Sys().(*syscall.Stat_t)
	if owner.Uid != fs.owner {
		return nil, errors.New("部署配置必须属于 root")
	}
	file, err := os.OpenFile(fs.resolve(rule.path), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("部署配置无法打开")
	}
	defer func() {
		if file.Close() != nil {
			result = errors.Join(result, errors.New("部署配置关闭失败"))
		}
	}()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.New("部署配置在读取前发生变化")
	}
	encoded, err = io.ReadAll(io.LimitReader(file, rule.maximum+1))
	after, statErr := file.Stat()
	if err != nil || statErr != nil || int64(len(encoded)) != before.Size() || after.Size() != before.Size() || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
		return nil, errors.New("部署配置无法完整读取或发生变化")
	}
	return encoded, nil
}
