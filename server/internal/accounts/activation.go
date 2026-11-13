package accounts

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/identity"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
)

// Activation 只能通过受保护的专用界面展示一次；查询用户或操作状态不会返回它。
type Activation struct {
	UserID    string              `json:"user_id"`
	Value     identity.Credential `json:"activation_token"`
	ExpiresAt time.Time           `json:"expires_at"`
}

// IssueActivation 仅对已经同步供给电脑的待激活用户签发材料；再次签发原子废止旧值。
func (service *Service) IssueActivation(ctx context.Context, actor identity.Principal, userID, requestID string) (Activation, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, true); err != nil {
		return Activation{}, err
	}
	return service.issueActivation(ctx, actor.UserID, userID, requestID)
}

func (service *Service) issueActivation(ctx context.Context, actorID, userID, requestID string) (Activation, error) {
	for _, id := range []string{userID, requestID} {
		if _, err := ulid.ParseStrict(id); err != nil {
			return Activation{}, ErrInvalidInput
		}
	}
	value, err := secure.Token()
	if err != nil {
		return Activation{}, errors.New("生成用户激活材料失败")
	}
	digest := sha256.Sum256([]byte(value))
	activation := Activation{UserID: userID, Value: identity.Credential(value)}
	err = service.transaction(ctx, func(transaction *sql.Tx) error {
		err := transaction.QueryRowContext(ctx, `SELECT u.id FROM tombot.users u JOIN tombot.computers c ON c.user_id=u.id
			JOIN tombot.user_creation_operations o ON o.user_id=u.id WHERE u.id=$1 AND u.status='pending_activation' AND c.status='ready' AND o.status='succeeded'
			FOR UPDATE OF u`, userID).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrActivationUnavailable
		}
		if err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.activation_tokens SET consumed_at=transaction_timestamp() WHERE user_id=$1 AND consumed_at IS NULL`, userID); err != nil {
			return err
		}
		err = transaction.QueryRowContext(ctx, `INSERT INTO tombot.activation_tokens(id,user_id,token_digest,expires_at)
			VALUES($1,$2,$3,transaction_timestamp()+$4::bigint*interval '1 millisecond') RETURNING expires_at`, ids.New(), userID, digest[:], service.options.ActivationLifetime.Milliseconds()).Scan(&activation.ExpiresAt)
		if err != nil {
			return err
		}
		return audit(ctx, transaction, actorID, userID, "签发用户激活材料", requestID, "succeeded", map[string]any{"expires_at": activation.ExpiresAt})
	})
	if err != nil {
		return Activation{}, err
	}
	return activation, nil
}
