package identity

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
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

// BeginAdminEnrollment 仅凭未消费的管理员激活值读取初始化材料；重复请求返回同一份材料。
// 原始材料只供激活界面展示，持久层保存与激活记录绑定的认证密文。
func (service *Service) BeginAdminEnrollment(ctx context.Context, token string, attempt Attempt) (Credential, error) {
	if !validToken(token) {
		return "", ErrInvalidActivation
	}
	if err := service.limitAttempt(ctx, "activation:"+secure.Hash(token), attempt); err != nil {
		return "", err
	}
	var enrollment Credential
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		activation, err := lockActivation(ctx, transaction, token)
		if err != nil {
			return err
		}
		if activation.role != "admin" {
			return ErrForbidden
		}
		var ciphertext, nonce []byte
		err = transaction.QueryRowContext(ctx, `SELECT ciphertext,nonce FROM tombot.activation_mfa WHERE activation_id=$1 AND user_id=$2`, activation.id, activation.userID).Scan(&ciphertext, &nonce)
		if errors.Is(err, sql.ErrNoRows) {
			secret, err := secure.Random(20)
			if err != nil {
				return err
			}
			defer clear(secret)
			ciphertext, nonce, err = secure.Seal(service.options.EncryptionKey, secret, activation.id)
			if err != nil {
				return err
			}
			if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.activation_mfa(activation_id,user_id,ciphertext,nonce) VALUES($1,$2,$3,$4)`, activation.id, activation.userID, ciphertext, nonce); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		secret, err := secure.Open(service.options.EncryptionKey, ciphertext, nonce, activation.id)
		if err != nil {
			return err
		}
		defer clear(secret)
		enrollment = Credential(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret))
		return audit(ctx, transaction, activation.userID, "", "准备管理员验证", "succeeded", attempt.RequestID)
	})
	if err != nil {
		return "", err
	}
	return enrollment, nil
}

func (service *Service) enrollMFA(ctx context.Context, transaction *sql.Tx, activation activationRecord, code string) error {
	var ciphertext, nonce []byte
	err := transaction.QueryRowContext(ctx, `SELECT ciphertext,nonce FROM tombot.activation_mfa WHERE activation_id=$1 AND user_id=$2`, activation.id, activation.userID).Scan(&ciphertext, &nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidCredentials
	}
	if err != nil {
		return err
	}
	secret, err := secure.Open(service.options.EncryptionKey, ciphertext, nonce, activation.id)
	if err != nil {
		return err
	}
	defer clear(secret)
	step, valid := verifyTOTP(secret, code, activation.now, -1)
	if !valid {
		return ErrInvalidCredentials
	}
	ciphertext, nonce, err = secure.Seal(service.options.EncryptionKey, secret, activation.userID)
	if err != nil {
		return err
	}
	if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.user_mfa(user_id,ciphertext,nonce,last_step) VALUES($1,$2,$3,$4)`, activation.userID, ciphertext, nonce, step); err != nil {
		return err
	}
	_, err = transaction.ExecContext(ctx, `DELETE FROM tombot.activation_mfa WHERE activation_id=$1 AND user_id=$2`, activation.id, activation.userID)
	return err
}

// verifyMFA 必须在持有用户行锁时调用，与登录、改密及二次校验共享重放计数。
func (service *Service) verifyMFA(ctx context.Context, transaction *sql.Tx, userID, code string, now time.Time) error {
	var ciphertext, nonce []byte
	var lastStep int64
	err := transaction.QueryRowContext(ctx, `SELECT ciphertext,nonce,last_step FROM tombot.user_mfa WHERE user_id=$1 FOR UPDATE`, userID).Scan(&ciphertext, &nonce, &lastStep)
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
	_, err = transaction.ExecContext(ctx, `UPDATE tombot.user_mfa SET last_step=$2 WHERE user_id=$1`, userID, step)
	return err
}
