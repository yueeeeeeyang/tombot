package identity

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	secure "io.yueyang/tom/internal/infra/crypto"
)

// totpCode 遵循 RFC 6238 的 30 秒时间步；SHA-1 在此仅用于标准 HMAC 协议。
func totpCode(secret []byte, step int64) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	if _, err := mac.Write(counter[:]); err != nil {
		panic("计算动态验证信息失败")
	}
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 15
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1000000)
}

func verifyTOTP(secret []byte, code string, now time.Time, lastStep int64) (int64, bool) {
	if len(secret) != 20 || len(code) != 6 {
		return 0, false
	}
	step := now.Unix() / 30
	accepted := int64(-1)
	for _, candidate := range []int64{step - 1, step, step + 1} {
		if subtle.ConstantTimeCompare([]byte(totpCode(secret, candidate)), []byte(code)) == 1 && candidate > lastStep {
			accepted = candidate
		}
	}
	return accepted, accepted >= 0
}

// verifyMFA 必须在持有身份记录锁时调用，与登录、改密及二次校验共享重放计数。
// 动态验证只存在于管理员身份表；普通用户没有该材料的读取路径。
func (service *Service) verifyMFA(ctx context.Context, transaction *sql.Tx, subject subject, userID, code string, now time.Time) error {
	var ciphertext, nonce []byte
	var lastStep int64
	err := transaction.QueryRowContext(ctx, subject.render(`SELECT ciphertext,nonce,last_step FROM {mfa} WHERE {owner}=$1 FOR UPDATE`), userID).Scan(&ciphertext, &nonce, &lastStep)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidCredentials
	}
	if err != nil {
		return err
	}
	secret, err := secure.Open(service.options.EncryptionKey, ciphertext, nonce, userID)
	if err != nil {
		return err
	}
	defer clear(secret)
	step, valid := verifyTOTP(secret, code, now, lastStep)
	if !valid {
		return ErrInvalidCredentials
	}
	_, err = transaction.ExecContext(ctx, subject.render(`UPDATE {mfa} SET last_step=$2 WHERE {owner}=$1`), userID, step)
	return err
}
