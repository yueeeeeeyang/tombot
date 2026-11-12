package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/crypto/argon2"
)

// Random 返回使用系统熵生成的随机字节；失败时不产生可预测的替代值。
func Random(size int) ([]byte, error) {
	value := make([]byte, size)
	_, err := rand.Read(value)
	return value, err
}

// Token 返回仅供当前调用传递的随机凭据；调用方不得记录明文。
func Token() (string, error) {
	value, err := Random(32)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

// Hash 用于随机高熵凭据的不可逆索引，不用于用户口令。
func Hash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest)
}

// HashPassword 用固定 Argon2id 成本派生口令；返回值包含随机盐。
func HashPassword(password string) (string, error) {
	salt, err := Random(16)
	if err != nil {
		return "", err
	}
	digest := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return base64.RawStdEncoding.EncodeToString(salt) + "." + base64.RawStdEncoding.EncodeToString(digest), nil
}

// ValidPasswordHash 验证持久口令派生值格式；损坏文件必须在启动时暴露。
func ValidPasswordHash(encoded string) bool {
	parts := strings.Split(encoded, ".")
	if len(parts) != 2 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[0])
	if err != nil || len(salt) != 16 {
		return false
	}
	digest, err := base64.RawStdEncoding.DecodeString(parts[1])
	return err == nil && len(digest) == 32
}

// VerifyPassword 对派生值作常量时间比较；损坏的编码一律拒绝。
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, ".")
	if len(parts) != 2 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[0])
	if err != nil || len(salt) != 16 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil || len(expected) != 32 {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

// ReadKey 只读取现有主密钥，备份或还原时缺失必须报错，避免为已有密文生成新密钥。
func ReadKey(dir, environment string) (key []byte, result error) {
	if environment != "" {
		key, err := base64.StdEncoding.DecodeString(environment)
		if err != nil || len(key) != 32 {
			clear(key)
			return nil, fmt.Errorf("主密钥编码或长度无效")
		}
		return key, nil
	}
	file, err := os.OpenFile(filepath.Join(dir, "master.key"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() != 32 || stat.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("主密钥文件类型、权限或长度无效")
	}
	key = make([]byte, 32)
	if _, err := io.ReadFull(file, key); err != nil {
		clear(key)
		return nil, err
	}
	return key, nil
}

// LoadKey 为首次启动创建主密钥，已存在的文件始终按只读校验规则处理。
func LoadKey(dir, environment string) ([]byte, error) {
	key, err := ReadKey(dir, environment)
	if environment != "" || !errors.Is(err, os.ErrNotExist) {
		return key, err
	}
	key, err = Random(32)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "master.key")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		clear(key)
		if errors.Is(err, os.ErrExist) {
			return ReadKey(dir, "")
		}
		return nil, err
	}
	_, writeErr := file.Write(key)
	if err = errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		clear(key)
		return nil, errors.Join(err, os.Remove(path))
	}
	return key, nil
}

// Seal 使用记录标识作为附加认证信息，避免密文在记录间被替换。
func Seal(key, plaintext []byte, recordID string) ([]byte, []byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce, err := Random(gcm.NonceSize())
	if err != nil {
		return nil, nil, err
	}
	return gcm.Seal(nil, nonce, plaintext, []byte(recordID)), nonce, nil
}

// Open 只向调用方返回认证通过的明文；调用方使用结束后应清零。
func Open(key, ciphertext, nonce []byte, recordID string) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("密文随机数长度无效")
	}
	return gcm.Open(nil, nonce, ciphertext, []byte(recordID))
}
