package deployment

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"io.yueyang/tom/internal/serverapp"
)

func (installer serverInstaller) serverCertificate(configuration serverapp.Config, api account) (*x509.Certificate, error) {
	encoded, err := installer.fs.readServerFile(fileRule{path: configuration.TLSCertificateFile, maximum: 1 << 20, api: true}, api)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(encoded)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("HTTPS 证书内容无效")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, errors.New("HTTPS 证书解析失败")
	}
	return leaf, nil
}

// checkHTTPSUpgrade 在停服务前验证修复范围，不把机构签发的证书替换成自签名身份。
func (installer serverInstaller) checkHTTPSUpgrade(configuration serverapp.Config, api account, renew bool) error {
	leaf, err := installer.serverCertificate(configuration, api)
	if err != nil {
		return err
	}
	if renew {
		if leaf.IsCA || leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) != nil {
			return errors.New("--renew-https 只重新签发自签名服务证书，不替换证书机构签发的证书")
		}
		return nil
	}
	return validateBrowserCertificate(leaf)
}

// renewServerHTTPS 在已核验备份之后发布完整证书目录，再原子切换配置中的成对引用。
// 路径绑定升级快照，恢复时复用同一证书；不会出现新证书配旧私钥的中间状态。
func (installer serverInstaller) renewServerHTTPS(ctx context.Context, configuration serverapp.Config, api account, journal serverUpgrade) (serverapp.Config, error) {
	if journal.Phase != "backed_up" && journal.Phase != "replaced" {
		return configuration, errors.New("HTTPS 重新签发缺少完整升级备份")
	}
	if err := ctx.Err(); err != nil {
		return configuration, err
	}
	origin, err := url.Parse(configuration.AdminOrigin)
	if err != nil {
		return configuration, errors.New("HTTPS 重新签发的服务地址无效")
	}
	directory := serverDirectory + "/https-" + contentDigest([]byte(journal.Backup))
	if _, err := os.Lstat(installer.fs.resolve(directory)); errors.Is(err, os.ErrNotExist) {
		if err := installer.publishServerCertificate(ctx, directory, origin.Hostname(), api); err != nil {
			return configuration, err
		}
	} else if err != nil {
		return configuration, errors.New("HTTPS 重新签发目录不可检查")
	}
	if err := installer.fs.directory(directory, api, false, true, false); err != nil {
		return configuration, err
	}
	certificatePath, keyPath := directory+"/certificate.pem", directory+"/tls.key"
	certificate, err := installer.fs.readServerFile(fileRule{path: certificatePath, maximum: 1 << 20, api: true}, api)
	if err != nil {
		return configuration, err
	}
	key, err := installer.fs.readServerFile(fileRule{path: keyPath, maximum: 64 << 10, private: true, api: true}, api)
	if err != nil {
		return configuration, err
	}
	defer clear(key)
	leaf, err := validateServerCertificate(certificate, key, origin.Hostname(), false)
	if err != nil {
		return configuration, err
	}
	if err := validateBrowserCertificate(leaf); err != nil {
		return configuration, err
	}
	if configuration.TLSCertificateFile != certificatePath || configuration.TLSPrivateKeyFile != keyPath {
		encoded, err := installer.fs.readServerFile(fileRule{path: serverConfigPath, maximum: 128 << 10, api: true}, api)
		if err != nil {
			return configuration, err
		}
		updated, err := updateServerTLSPaths(encoded, certificatePath, keyPath)
		if err != nil {
			return configuration, err
		}
		if err := installer.replaceServerFile(serverConfigPath, updated, 0644); err != nil {
			return configuration, err
		}
	}
	return installer.loadServerConfig(api)
}

func updateServerTLSPaths(encoded []byte, certificate, key string) ([]byte, error) {
	configuration, err := serverapp.ParseConfig(encoded)
	if err != nil {
		return nil, err
	}
	configuration.TLSCertificateFile, configuration.TLSPrivateKeyFile = certificate, key
	updated, err := json.MarshalIndent(configuration, "", "  ")
	if err != nil {
		return nil, errors.New("编码 HTTPS 配置失败")
	}
	if _, err := serverapp.ParseConfig(updated); err != nil {
		return nil, err
	}
	return updated, nil
}

func (installer serverInstaller) publishServerCertificate(ctx context.Context, destination, host string, api account) (result error) {
	if err := installer.fs.directory(serverDirectory, api, false, true, false); err != nil {
		return err
	}
	certificate, key, err := createServerCertificate(host, false, time.Now())
	if err != nil {
		return err
	}
	defer clear(key)
	stage, err := os.MkdirTemp(installer.fs.resolve(serverDirectory), ".https-")
	if err != nil {
		return errors.New("创建 HTTPS 证书暂存目录失败")
	}
	defer func() {
		if err := os.RemoveAll(stage); err != nil {
			result = errors.Join(result, errors.New("清理 HTTPS 证书暂存目录失败"))
		}
	}()
	for _, file := range []configurationFile{{name: "certificate.pem", content: certificate, mode: 0644}, {name: "tls.key", content: key, mode: 0600}} {
		path := filepath.Join(stage, file.name)
		if err := writeConfigurationFile(path, file.content, file.mode); err != nil {
			return err
		}
		if err := os.Chown(path, int(api.uid), int(api.gid)); err != nil {
			return errors.New("设置 HTTPS 证书身份失败")
		}
	}
	if err := os.Chmod(stage, 0755); err != nil {
		return errors.New("设置 HTTPS 证书目录权限失败")
	}
	if err := syncDirectory(stage); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := publishPath(stage, installer.fs.resolve(destination)); err != nil {
		return errors.New("发布 HTTPS 证书失败，未覆盖已有身份")
	}
	return syncDirectory(installer.fs.resolve(serverDirectory))
}
