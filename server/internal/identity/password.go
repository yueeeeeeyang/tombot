package identity

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const passwordParameters = "$argon2id$v=19$m=65536,t=3,p=2$"

func validatePassword(password string) error {
	if !utf8.ValidString(password) || utf8.RuneCountInString(password) < 12 || len(password) > 1024 {
		return ErrInvalidInput
	}
	return nil
}

// hashPassword 将算法及成本写入编码，未知格式明确报错，不猜测或兼容旧凭据。
func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", errors.New("生成口令盐失败")
	}
	digest := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	defer clear(digest)
	return passwordParameters + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(digest), nil
}

func verifyPassword(encoded, password string) (bool, error) {
	if !strings.HasPrefix(encoded, passwordParameters) {
		return false, errors.New("口令派生算法或成本无效")
	}
	parts := strings.Split(strings.TrimPrefix(encoded, passwordParameters), "$")
	if len(parts) != 2 {
		return false, errors.New("口令派生编码损坏")
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[0])
	if err != nil || len(salt) != 16 {
		return false, errors.New("口令盐编码损坏")
	}
	expected, err := base64.RawStdEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(expected) != 32 {
		return false, errors.New("口令摘要编码损坏")
	}
	actual := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	defer clear(actual)
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}

// Credential 阻止调试格式化意外输出凭据；JSON 序列化只允许专用认证响应使用。
type Credential string

func (Credential) String() string   { return "[已隐藏]" }
func (Credential) GoString() string { return "[已隐藏]" }
func (Credential) Format(state fmt.State, _ rune) {
	if _, err := state.Write([]byte("[已隐藏]")); err != nil {
		panic("格式化脱敏凭据失败")
	}
}
