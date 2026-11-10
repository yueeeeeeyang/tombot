package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/oklog/ulid/v2"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store"
)

const pendingName = ".auth-setup.pending"

type setupFile struct {
	credentials
	SetupID string `json:"setup_id,omitempty"`
}

func readSetup(path string) (record setupFile, result error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return record, err
	}
	defer func() { result = errors.Join(result, f.Close()) }()
	stat, err := f.Stat()
	if err != nil {
		return record, err
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 || stat.Size() > 4096 {
		return record, fmt.Errorf("认证文件类型、权限或长度无效")
	}
	encoded, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return record, err
	}
	if len(encoded) > 4096 || json.Unmarshal(encoded, &record) != nil || !secure.ValidPasswordHash(record.PasswordHash) {
		return record, fmt.Errorf("认证文件损坏")
	}
	if record.SetupID != "" {
		if _, err := ulid.ParseStrict(record.SetupID); err != nil {
			return record, fmt.Errorf("认证初始化标识损坏")
		}
	}
	return record, nil
}

func syncSetupDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// setupState 同时读取凭据和初始化提交标记。旧实例的认证文件会保留原有身份，只有新初始化需要提交标记。
func setupState(ctx context.Context, db *sql.DB, dir string) (record setupFile, pending bool, err error) {
	var committed string
	err = db.QueryRowContext(ctx, "SELECT json_extract(value_json,'$') FROM system_state WHERE key='auth.setup'").Scan(&committed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return record, false, err
	}
	record, err = readSetup(filepath.Join(dir, "auth.json"))
	if err == nil {
		if record.SetupID != committed {
			return record, false, fmt.Errorf("认证文件与数据库的初始化标识不一致")
		}
		return record, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return record, false, err
	}
	if committed == "" {
		var used bool
		if err = db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM devices) OR EXISTS(SELECT 1 FROM bots) OR EXISTS(SELECT 1 FROM secrets) OR EXISTS(SELECT 1 FROM providers)").Scan(&used); err != nil {
			return record, false, err
		}
		if used {
			return record, false, fmt.Errorf("已有业务记录但缺少认证文件")
		}
		return setupFile{}, false, nil
	}
	record, err = readSetup(filepath.Join(dir, pendingName))
	if err != nil {
		return record, false, fmt.Errorf("已提交初始化但缺少有效凭据：%w", err)
	}
	if record.SetupID != committed {
		return record, false, fmt.Errorf("暂存认证文件与初始化标识不一致")
	}
	return record, true, nil
}

// ValidateSetup 只读验证归档时的初始化状态。已提交但未发布的暂存凭据也可以在恢复实例启动时完成发布。
func ValidateSetup(ctx context.Context, db *sql.DB, dir string) (bool, error) {
	record, _, err := setupState(ctx, db, dir)
	return record.PasswordHash != "", err
}

// loadSetup 先按已提交标记恢复凭据，正常情况下不会重复写审计或签发初始化码。
func (s *Service) loadSetup(ctx context.Context) error {
	dir := filepath.Dir(s.path)
	record, pending, err := setupState(ctx, s.db, dir)
	if err != nil {
		return err
	}
	name := filepath.Join(dir, pendingName)
	if pending {
		if err = os.Rename(name, s.path); err != nil {
			return err
		}
		if err = syncSetupDir(dir); err != nil {
			return err
		}
	} else if err = os.Remove(name); err == nil {
		if err = syncSetupDir(dir); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	s.passwordHash = record.PasswordHash
	if record.PasswordHash != "" {
		s.setupHash = ""
	}
	return nil
}

// commitSetup 先将可恢复的凭据写入私有文件，再把授权标记和审计一次提交；进程终止后按标记判定结果。
func (s *Service) commitSetup(ctx context.Context, hash string) error {
	dir := filepath.Dir(s.path)
	id := ids.New()
	encoded, err := json.Marshal(setupFile{credentials{hash}, id})
	if err != nil {
		return err
	}
	name := filepath.Join(dir, pendingName)
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(encoded)
	if err = errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return err
	}
	if err = syncSetupDir(dir); err != nil {
		return err
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO system_state(key,value_json,updated_at) VALUES('auth.setup',json_quote(?),?)", id, s.now().UnixMilli())
		if err != nil {
			return err
		}
		return store.Audit(ctx, tx, "user", "auth.setup", id, struct{}{})
	})
	if err != nil {
		return err
	}
	return s.loadSetup(ctx)
}
