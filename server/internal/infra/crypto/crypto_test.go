package crypto

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptionAuthenticatesRecordAndCiphertext(t *testing.T) {
	key, err := Random(32)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Random(100)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, nonce, err := Seal(key, plain, "record")
	if err != nil {
		t.Fatal(err)
	}
	actual, err := Open(key, encrypted, nonce, "record")
	if err != nil || !bytes.Equal(actual, plain) {
		t.Fatal("解密不一致")
	}
	if _, err = Open(key, encrypted, nonce, "other"); err == nil {
		t.Fatal("记录替换未被发现")
	}
	encrypted[0] ^= 1
	if _, err = Open(key, encrypted, nonce, "record"); err == nil {
		t.Fatal("篡改未被发现")
	}
	if _, err = Open(key, encrypted, nil, "record"); err == nil {
		t.Fatal("错误随机数被接受")
	}
}
func TestKeyPersistenceAndPermissions(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadKey(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadKey(dir, "")
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("主密钥重启后改变")
	}
	if err = os.Chmod(dir+"/master.key", 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadKey(dir, ""); err == nil {
		t.Fatal("过宽权限必须阻止读取")
	}
}
func TestPasswordHash(t *testing.T) {
	password, err := Token()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, password) || VerifyPassword(hash, password+"x") || VerifyPassword("invalid", password) {
		t.Fatal("口令验证错误")
	}
}

func TestReadKeyRefusesMissingOrIndirectFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadKey(dir, ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("读取缺失密钥应明确失败")
	}
	name := filepath.Join(dir, "master.key")
	if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("只读操作创建了密钥")
	}
	other := t.TempDir()
	key, err := LoadKey(other, "")
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	if err = os.Symlink(filepath.Join(other, "master.key"), name); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadKey(dir, ""); err == nil {
		t.Fatal("文件密钥链接被接受")
	}
	if _, err = ReadKey(dir, "invalid"); err == nil {
		t.Fatal("环境密钥格式校验失效")
	}
}
