package serverapp

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// readFile 不输出文件内容和系统原始错误；拒绝链接、非普通文件、过宽权限和非部署身份的文件。
func readFile(path string, maximum int, private bool) (content []byte, failure error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || maximum < 1 {
		return nil, errors.New("配置文件路径或大小限制无效")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("服务配置或凭据文件无法打开")
	}
	defer func() {
		if file.Close() != nil {
			failure = errors.Join(failure, errors.New("关闭服务配置文件失败"))
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, errors.New("服务配置文件状态不可读取")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 && owner.Uid != uint32(os.Geteuid()) || owner.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || private && info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > int64(maximum) {
		return nil, errors.New("服务配置或凭据文件身份、权限或大小无效")
	}
	content, err = io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil || len(content) == 0 || len(content) > maximum {
		return nil, errors.New("读取服务配置文件失败或超过限制")
	}
	return content, nil
}

func readKey(path string) ([]byte, error) {
	key, err := readFile(path, 32, true)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		clear(key)
		return nil, errors.New("服务密钥文件必须包含 32 字节原始内容")
	}
	return key, nil
}

func loadTLS(configuration Config) (*tls.Config, error) {
	certificate, err := readFile(configuration.TLSCertificateFile, 1<<20, false)
	if err != nil {
		return nil, err
	}
	privateKey, err := readFile(configuration.TLSPrivateKeyFile, 64<<10, true)
	if err != nil {
		return nil, err
	}
	defer clear(privateKey)
	pair, err := tls.X509KeyPair(certificate, privateKey)
	if err != nil || len(pair.Certificate) == 0 {
		return nil, errors.New("服务 TLS 证书或私钥无效")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, errors.New("服务 TLS 证书无法解析")
	}
	origin, err := url.Parse(configuration.AdminOrigin)
	if err != nil || leaf.VerifyHostname(origin.Hostname()) != nil || time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		return nil, errors.New("服务 TLS 证书与管理来源或有效期不符")
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13}, nil
}
