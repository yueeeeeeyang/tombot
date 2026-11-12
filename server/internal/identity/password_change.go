package identity

import (
	"context"
	"database/sql"
)

func (service *Service) sessionCredential(ctx context.Context, principal Principal) (string, error) {
	var hash string
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		if _, _, err := lockSession(ctx, transaction, principal); err != nil {
			return err
		}
		return transaction.QueryRowContext(ctx, `SELECT password_hash FROM tombot.user_credentials WHERE user_id=$1`, principal.UserID).Scan(&hash)
	})
	return hash, err
}

// verifyCurrentCredential 防止口令校验与提交之间发生改密或设备撤销；管理员验证计数也参与同一事务。
func (service *Service) verifyCurrentCredential(ctx context.Context, transaction *sql.Tx, principal Principal, expectedHash, code string) error {
	session, now, err := lockSession(ctx, transaction, principal)
	if err != nil {
		return err
	}
	var currentHash string
	if err = transaction.QueryRowContext(ctx, `SELECT password_hash FROM tombot.user_credentials WHERE user_id=$1`, principal.UserID).Scan(&currentHash); err != nil {
		return err
	}
	if currentHash != expectedHash {
		return ErrInvalidCredentials
	}
	if session.Principal.Role == "admin" {
		return service.verifyMFA(ctx, transaction, principal.UserID, code, now)
	}
	return nil
}

// Reauthenticate 重新验证口令与管理员动态信息，为敏感管理操作设置有时限的验证窗口。
func (service *Service) Reauthenticate(ctx context.Context, principal Principal, password, code string, attempt Attempt) error {
	if len(password) > 1024 {
		return ErrInvalidInput
	}
	if err := service.limitAttempt(ctx, "verify:"+principal.UserID, attempt); err != nil {
		return err
	}
	hash, err := service.sessionCredential(ctx, principal)
	if err != nil {
		return err
	}
	var valid bool
	if err = service.passwordWork(ctx, func() error { var err error; valid, err = verifyPassword(hash, password); return err }); err != nil {
		return err
	}
	if !valid {
		return ErrInvalidCredentials
	}
	return service.transaction(ctx, func(transaction *sql.Tx) error {
		if err := service.verifyCurrentCredential(ctx, transaction, principal, hash, code); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE tombot.user_sessions SET verified_at=transaction_timestamp() WHERE user_id=$1 AND id=$2`, principal.UserID, principal.SessionID); err != nil {
			return err
		}
		return audit(ctx, transaction, principal.UserID, principal.SessionID, "再次验证身份", "succeeded", attempt.RequestID)
	})
}

// ChangePassword 在同一事务中更换口令、增加认证版本并撤销全部设备与会话；调用方必须重新登录。
func (service *Service) ChangePassword(ctx context.Context, principal Principal, currentPassword, newPassword, code string, attempt Attempt) error {
	if len(currentPassword) > 1024 {
		return ErrInvalidInput
	}
	if err := validatePassword(newPassword); err != nil {
		return err
	}
	if err := service.limitAttempt(ctx, "verify:"+principal.UserID, attempt); err != nil {
		return err
	}
	hash, err := service.sessionCredential(ctx, principal)
	if err != nil {
		return err
	}
	var encoded string
	err = service.passwordWork(ctx, func() error {
		valid, err := verifyPassword(hash, currentPassword)
		if err != nil {
			return err
		}
		if !valid {
			return ErrInvalidCredentials
		}
		encoded, err = hashPassword(newPassword)
		return err
	})
	if err != nil {
		return err
	}
	return service.transaction(ctx, func(transaction *sql.Tx) error {
		if err := service.verifyCurrentCredential(ctx, transaction, principal, hash, code); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE tombot.user_credentials SET password_hash=$2,changed_at=transaction_timestamp() WHERE user_id=$1`, principal.UserID, encoded); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE tombot.users SET auth_version=auth_version+1,revision=revision+1,updated_at=transaction_timestamp() WHERE id=$1`, principal.UserID); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE tombot.user_sessions SET revoked_at=transaction_timestamp() WHERE user_id=$1 AND revoked_at IS NULL`, principal.UserID); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE tombot.devices SET revoked_at=transaction_timestamp() WHERE user_id=$1 AND revoked_at IS NULL`, principal.UserID); err != nil {
			return err
		}
		return audit(ctx, transaction, principal.UserID, principal.SessionID, "修改口令", "succeeded", attempt.RequestID)
	})
}
