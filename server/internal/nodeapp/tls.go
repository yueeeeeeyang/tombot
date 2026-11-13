package nodeapp

import (
	"crypto/tls"
	"errors"
	"io"
	"os"
	"syscall"

	"io.yueyang/tom/internal/nodeapi"
)

// loadControlTLS 读取节点证书与私钥并构造双向认证配置。
// 私钥要求仅所有者可读且不接受符号链接；公钥证书允许多一个读位，便于部署核对指纹。
func loadControlTLS(configuration ControlTLS) (*tls.Config, error) {
	certificate, err := readProtectedFile(configuration.CertificateFile, 1<<20, false)
	if err != nil {
		return nil, err
	}
	privateKey, err := readProtectedFile(configuration.PrivateKeyFile, 64<<10, true)
	if err != nil {
		return nil, err
	}
	defer clear(privateKey)
	pair, err := tls.X509KeyPair(certificate, privateKey)
	if err != nil {
		return nil, errors.New("节点控制证书或私钥无效")
	}
	fingerprints, err := nodeapi.ParseFingerprints(configuration.ClientFingerprints)
	if err != nil {
		return nil, err
	}
	return nodeapi.ServerTLS(pair, fingerprints)
}

func readProtectedFile(path string, maximum int64, private bool) (content []byte, failure error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("节点控制证书文件无法打开")
	}
	defer func() {
		if err := file.Close(); err != nil {
			failure = errors.Join(failure, errors.New("关闭节点控制证书文件失败"))
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, errors.New("节点控制证书文件状态不可读取")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 && owner.Uid != uint32(os.Geteuid()) || owner.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || private && info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > maximum {
		return nil, errors.New("节点控制证书文件身份、权限或大小无效")
	}
	content, err = io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || len(content) == 0 || int64(len(content)) > maximum {
		return nil, errors.New("读取节点控制证书文件失败或超过限制")
	}
	return content, nil
}
