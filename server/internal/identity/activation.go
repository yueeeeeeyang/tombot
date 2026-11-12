package identity

import (
	"context"
	"database/sql"
	"errors"
	"time"

	secure "io.yueyang/tom/internal/infra/crypto"
)

type activationRecord struct {
	id     string
	userID string
	role   string
	now    time.Time
}

// lockActivation 统一先锁用户再锁激活记录，与其他身份操作保持同一锁顺序。
func lockActivation(ctx context.Context, transaction *sql.Tx, token string) (activationRecord, error) {
	var activation activationRecord
	err := transaction.QueryRowContext(ctx, `SELECT user_id FROM tombot.activation_tokens WHERE token_digest=$1`, digest(token)).Scan(&activation.userID)
	if errors.Is(err, sql.ErrNoRows) {
		return activation, ErrInvalidActivation
	}
	if err != nil {
		return activation, err
	}
	err = transaction.QueryRowContext(ctx, `SELECT role FROM tombot.users WHERE id=$1 AND status='pending_activation' FOR UPDATE`, activation.userID).Scan(&activation.role)
	if errors.Is(err, sql.ErrNoRows) {
		return activation, ErrInvalidActivation
	}
	if err != nil {
		return activation, err
	}
	err = transaction.QueryRowContext(ctx, `SELECT id,transaction_timestamp() FROM tombot.activation_tokens
		WHERE user_id=$1 AND token_digest=$2 AND consumed_at IS NULL AND expires_at>transaction_timestamp() FOR UPDATE`, activation.userID, digest(token)).Scan(&activation.id, &activation.now)
	if errors.Is(err, sql.ErrNoRows) {
		return activation, ErrInvalidActivation
	}
	return activation, err
}

// Activate 消费一次性激活值并原子保存口令；管理员还必须完成动态验证登记。
// 不隐式签发会话，用户需使用自己的凭据登录；错误或并发重复请求均不消耗有效激活值。
func (service *Service) Activate(ctx context.Context, token, password, code string, attempt Attempt) error {
	if !validToken(token) {
		return ErrInvalidActivation
	}
	if err := validatePassword(password); err != nil {
		return err
	}
	if err := service.limitAttempt(ctx, "activation:"+secure.Hash(token), attempt); err != nil {
		return err
	}
	// 在昂贵计算前检查激活值；提交时仍重新锁定与验证，防止检查后的并发消费。
	if err := service.transaction(ctx, func(transaction *sql.Tx) error { _, err := lockActivation(ctx, transaction, token); return err }); err != nil {
		return err
	}
	var encoded string
	if err := service.passwordWork(ctx, func() error { var err error; encoded, err = hashPassword(password); return err }); err != nil {
		return err
	}
	return service.transaction(ctx, func(transaction *sql.Tx) error {
		activation, err := lockActivation(ctx, transaction, token)
		if err != nil {
			return err
		}
		if activation.role == "admin" {
			if err = service.enrollMFA(ctx, transaction, activation, code); err != nil {
				return err
			}
		}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.user_credentials(user_id,password_hash) VALUES($1,$2)`, activation.userID, encoded); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.users SET status='active',revision=revision+1,updated_at=transaction_timestamp() WHERE id=$1`, activation.userID); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.activation_tokens SET consumed_at=transaction_timestamp() WHERE id=$1 AND user_id=$2`, activation.id, activation.userID); err != nil {
			return err
		}
		return audit(ctx, transaction, activation.userID, "", "激活账户", "succeeded", attempt.RequestID)
	})
}
