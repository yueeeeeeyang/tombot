// 构建机专用的自签名发行工具，不随客户端分发。
package main

import (
	"context"
	"crypto"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/sassoftware/relic/v8/lib/authenticode"
	"github.com/sassoftware/relic/v8/lib/certloader"
	"software.sslmate.com/src/go-pkcs12"
)

// signFile 在打包暂存目录内签署 PE 文件，避免命令行及构建日志接触 PFX 口令。
func signFile(path, identity, password string) error {
	blob, err := os.ReadFile(identity)
	if err != nil {
		return err
	}
	key, leaf, _, err := pkcs12.DecodeChain(blob, password)
	if err != nil {
		return errors.New("无法读取 Windows 发行证书及私钥，请检查 PFX 和口令")
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return errors.New("不支持的 Windows 发行私钥")
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	if err := leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature); err != nil {
		return errors.New("本地发行模式需要自签名证书")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	digest, err := authenticode.DigestPE(file, crypto.SHA256, false)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	patch, _, err := digest.Sign(ctx, &certloader.Certificate{Leaf: leaf, PrivateKey: signer}, nil)
	if err != nil {
		return err
	}
	if err := patch.Apply(file, path); err != nil {
		return err
	}
	// 补丁库必要时会以新文件替换原路径，重新打开才能校验最终写入的文件。
	signed, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer signed.Close()
	return authenticode.FixPEChecksum(signed)
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "须指定待签名文件和发行证书路径")
		os.Exit(2)
	}
	if err := signFile(os.Args[1], os.Args[2], os.Getenv("WIN_CSC_KEY_PASSWORD")); err != nil {
		fmt.Fprintln(os.Stderr, "Windows 发行签名失败：", err)
		os.Exit(1)
	}
}
