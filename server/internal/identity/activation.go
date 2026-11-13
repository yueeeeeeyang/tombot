package identity

import (
	"context"
	"database/sql"
	"errors"
	"time"

	secure "io.yueyang/tom/internal/infra/crypto"
)

type activationRecord struct {
	id      string
	userID  string
	subject subject
	now     time.Time
}

// lockActivation 只查用户激活表：管理员在创建时直接设定口令，不走激活流程。
// 统一先锁身份记录再锁激活记录，与其他身份操作保持同一锁顺序。
func lockActivation(ctx context.Context, transaction *sql.Tx, token string) (activationRecord, error) {
	for _, subject := range []subject{userSubject} {
		var activation activationRecord
		err := transaction.QueryRowContext(ctx, subject.render(`SELECT {owner} FROM {activationTokens} WHERE token_digest=$1`), digest(token)).Scan(&activation.userID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return activation, err
		}
		activation.subject = subject
		err = transaction.QueryRowContext(ctx, subject.render(`SELECT id FROM {accounts} WHERE id=$1 AND status='pending_activation' FOR UPDATE`), activation.userID).Scan(&activation.id)
		if errors.Is(err, sql.ErrNoRows) {
			return activation, ErrInvalidActivation
		}
		if err != nil {
			return activation, err
		}
		err = transaction.QueryRowContext(ctx, subject.render(`SELECT id,transaction_timestamp() FROM {activationTokens}
			WHERE {owner}=$1 AND token_digest=$2 AND consumed_at IS NULL AND expires_at>transaction_timestamp() FOR UPDATE`), activation.userID, digest(token)).Scan(&activation.id, &activation.now)
		if errors.Is(err, sql.ErrNoRows) {
			return activation, ErrInvalidActivation
		}
		return activation, err
	}
	return activationRecord{}, ErrInvalidActivation
}

// Activate 消费一次性激活值并原子保存口令，仅作用于普通用户。
// 不隐式签发会话，账户需使用自己的凭据登录；错误或并发重复请求均不消耗有效激活值。
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
		subject := activation.subject
		// 普通用户不使用动态验证：该材料只属于管理员，而管理员在创建时已完成登记。
		_ = code
		if _, err = transaction.ExecContext(ctx, subject.render(`INSERT INTO {credentials}({owner},password_hash) VALUES($1,$2)`), activation.userID, encoded); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, subject.render(`UPDATE {accounts} SET status='active',revision=revision+1,updated_at=transaction_timestamp() WHERE id=$1`), activation.userID); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, subject.render(`UPDATE {activationTokens} SET consumed_at=transaction_timestamp() WHERE id=$1 AND {owner}=$2`), activation.id, activation.userID); err != nil {
			return err
		}
		return audit(ctx, transaction, activation.userID, "", "激活账户", "succeeded", attempt.RequestID)
	})
}
