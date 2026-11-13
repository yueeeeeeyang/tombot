package identity

import (
	"context"
	"database/sql"
)

// sessionCredential 读取当前会话所属身份的当前口令散列，用于比对后再决定是否换新口令。
// 普通用户与管理员分开取表，作用域缺失时按无效会话处理。
func (service *Service) sessionCredential(ctx context.Context, principal Principal) (string, error) {
	subject, ok := subjectForScope(principal.Scope)
	if !ok {
		return "", ErrInvalidSession
	}
	var hash string
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		if _, _, err := lockSession(ctx, transaction, principal); err != nil {
			return err
		}
		return transaction.QueryRowContext(ctx, subject.render(`SELECT password_hash FROM {credentials} WHERE {owner}=$1`), principal.UserID).Scan(&hash)
	})
	return hash, err
}

// verifyCurrentCredential 防止口令校验与提交之间发生改密或设备撤销；管理员验证计数也参与同一事务。
// 只有管理作用域要求动态验证：普通用户没有该材料，也不参与管理动作。
func (service *Service) verifyCurrentCredential(ctx context.Context, transaction *sql.Tx, principal Principal, expectedHash, code string) error {
	subject, ok := subjectForScope(principal.Scope)
	if !ok {
		return ErrInvalidSession
	}
	session, now, err := lockSession(ctx, transaction, principal)
	if err != nil {
		return err
	}
	var currentHash string
	if err = transaction.QueryRowContext(ctx, subject.render(`SELECT password_hash FROM {credentials} WHERE {owner}=$1`), principal.UserID).Scan(&currentHash); err != nil {
		return err
	}
	if currentHash != expectedHash {
		return ErrInvalidCredentials
	}
	if session.Principal.Scope != "admin" {
		return nil
	}
	return service.verifyMFA(ctx, transaction, administratorSubject, principal.UserID, code, now)
}

// Reauthenticate 重新验证口令与管理员动态信息，为敏感管理操作设置有时限的验证窗口。
func (service *Service) Reauthenticate(ctx context.Context, principal Principal, password, code string, attempt Attempt) error {
	subject, ok := subjectForScope(principal.Scope)
	if !ok {
		return ErrInvalidInput
	}
	if len(password) > 1024 {
		return ErrInvalidInput
	}
	if err := service.limitAttempt(ctx, "verify:"+principal.Scope+":"+principal.UserID, attempt); err != nil {
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
		if _, err := transaction.ExecContext(ctx, subject.render(`UPDATE {sessions} SET verified_at=transaction_timestamp() WHERE {owner}=$1 AND id=$2`), principal.UserID, principal.SessionID); err != nil {
			return err
		}
		return audit(ctx, transaction, principal.UserID, principal.SessionID, "再次验证身份", "succeeded", attempt.RequestID)
	})
}

// ChangePassword 在同一事务中更换口令、增加认证版本并撤销全部设备与会话；调用方必须重新登录。
func (service *Service) ChangePassword(ctx context.Context, principal Principal, currentPassword, newPassword, code string, attempt Attempt) error {
	subject, ok := subjectForScope(principal.Scope)
	if !ok {
		return ErrInvalidInput
	}
	if len(currentPassword) > 1024 {
		return ErrInvalidInput
	}
	if err := validatePassword(newPassword); err != nil {
		return err
	}
	if err := service.limitAttempt(ctx, "verify:"+principal.Scope+":"+principal.UserID, attempt); err != nil {
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
		if _, err := transaction.ExecContext(ctx, subject.render(`UPDATE {credentials} SET password_hash=$2,changed_at=transaction_timestamp() WHERE {owner}=$1`), principal.UserID, encoded); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, subject.render(`UPDATE {accounts} SET auth_version=auth_version+1,revision=revision+1,updated_at=transaction_timestamp() WHERE id=$1`), principal.UserID); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, subject.render(`UPDATE {sessions} SET revoked_at=transaction_timestamp() WHERE {owner}=$1 AND revoked_at IS NULL`), principal.UserID); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, subject.render(`UPDATE {devices} SET revoked_at=transaction_timestamp() WHERE {owner}=$1 AND revoked_at IS NULL`), principal.UserID); err != nil {
			return err
		}
		return audit(ctx, transaction, principal.UserID, principal.SessionID, "修改口令", "succeeded", attempt.RequestID)
	})
}
