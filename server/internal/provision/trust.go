package provision

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"
)

// checkSystemRoots 离线校验当前平台实际使用的根证书库。macOS 由原生验证器持有系统根，不能用 Subjects 枚举。
func checkSystemRoots(ctx context.Context, env Environment) error {
	roots, err := x509.SystemCertPool()
	if err != nil {
		return err
	}
	if roots == nil || roots.Equal(x509.NewCertPool()) {
		return fmt.Errorf("系统根证书为空")
	}
	if env.OS != "darwin" {
		return nil
	}
	anchors, err := env.Runner.Run(ctx, "/usr/bin/security", "find-certificate", "-a", "-p", "/System/Library/Keychains/SystemRootCertificates.keychain")
	if err != nil {
		return err
	}
	return verifyMacAnchors(ctx, env.Runner, anchors)
}

// verifyMacAnchors 使用系统基本信任策略并声明被检证书是 CA。TLS 服务器策略会把根错误地当作网站叶证书。
// 系统命令需要可寻址的普通文件，临时文件只承载公开证书，校验完成立即删除。
func verifyMacAnchors(ctx context.Context, runner Runner, anchors []byte) (result error) {
	file, err := os.CreateTemp("", "tom-public-root-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() { result = errors.Join(result, os.Remove(name)) }()
	if err = file.Close(); err != nil {
		return err
	}
	now := time.Now()
	var last error
	for len(anchors) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		block, rest := pem.Decode(anchors)
		if block == nil {
			break
		}
		anchors = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA || now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
			continue
		}
		if err = os.WriteFile(name, block.Bytes, 0600); err != nil {
			return err
		}
		if _, last = runner.Run(ctx, "/usr/bin/security", "verify-cert", "-l", "-L", "-q", "-p", "basic", "-c", name); last == nil {
			return nil
		}
	}
	return errors.Join(fmt.Errorf("系统信任库没有可验证的根证书"), last, ctx.Err())
}
