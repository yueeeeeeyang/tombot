// Windows 升级验签工具。信任起点是本机已安装的客户端，自签名证书只对本次校验生效。
package main

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"debug/pe"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/sassoftware/relic/v8/lib/authenticode"
)

// validateTable 在签名解析器分配内存前限定证书表，并拒绝文件尾部未签名的附加内容。
func validateTable(file *os.File) error {
	image, err := pe.NewFile(file)
	if err != nil {
		return err
	}
	var table pe.DataDirectory
	switch header := image.OptionalHeader.(type) {
	case *pe.OptionalHeader32:
		table = header.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_SECURITY]
	case *pe.OptionalHeader64:
		table = header.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_SECURITY]
	default:
		return errors.New("不支持的 Windows 可执行文件")
	}
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	if table.Size < 8 || table.Size > 4<<20 || int64(table.VirtualAddress)+int64(table.Size) != stat.Size() {
		return errors.New("Windows 文件缺少有效的签名证书表")
	}
	_, err = file.Seek(0, io.SeekStart)
	return err
}

// verifiedSigner 验证整个 PE 的摘要和签名，返回经过验证的 SHA-256 签名者。
func verifiedSigner(path string) (*x509.Certificate, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err = validateTable(file); err != nil {
		return nil, err
	}
	signatures, err := authenticode.VerifyPE(file, false)
	if err != nil {
		return nil, err
	}
	for _, signature := range signatures {
		cert := signature.Certificate
		if cert == nil || signature.ImageHashFunc != crypto.SHA256 || !slices.Contains(cert.ExtKeyUsage, x509.ExtKeyUsageCodeSigning) {
			continue
		}
		if bytes.Equal(cert.RawIssuer, cert.RawSubject) {
			if cert.KeyUsage&x509.KeyUsageDigitalSignature == 0 || cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature) != nil {
				continue
			}
			roots := x509.NewCertPool()
			roots.AddCert(cert)
			if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}, CurrentTime: time.Now()}); err != nil {
				continue
			}
		} else if err := signature.VerifyChain(nil, nil, x509.ExtKeyUsageCodeSigning); err != nil {
			continue
		}
		return cert, nil
	}
	return nil, errors.New("没有可用于代码签名的有效 SHA-256 发布者")
}

// verifyUpdate 只接受与已安装可执行文件具有完全相同签名证书的升级。
func verifyUpdate(installed, candidate string) error {
	publisher, err := verifiedSigner(installed)
	if err != nil {
		return fmt.Errorf("已安装客户端验签失败：%w", err)
	}
	signer, err := verifiedSigner(candidate)
	if err != nil {
		return fmt.Errorf("升级包验签失败：%w", err)
	}
	if !publisher.Equal(signer) {
		return errors.New("升级包的发布者证书与当前客户端不匹配")
	}
	return nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "须指定已安装客户端及待校验的升级包")
		os.Exit(2)
	}
	if err := verifyUpdate(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
