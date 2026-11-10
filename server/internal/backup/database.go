package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"net/url"

	"io.yueyang/tom/internal/auth"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/store"
	"modernc.org/sqlite"
)

// readDatabase 禁止创建、迁移原库。SQLite 自己合并已提交的 WAL 页面。
func readDatabase(filename string, immutable bool) (*sql.DB, error) {
	address := url.URL{Scheme: "file", Path: filename, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	if immutable {
		address.RawQuery += "&immutable=1"
	}
	db, err := sql.Open("sqlite", address.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// snapshot 按页复制以保留 rowid 和 FTS 对应关系，每批之间响应取消。
func snapshot(ctx context.Context, source, target string) (result error) {
	db, err := readDatabase(source, false)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, db.Close()) }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, conn.Close()) }()
	return conn.Raw(func(driver any) (result error) {
		engine, ok := driver.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("数据库驱动不支持快照")
		}
		address := url.URL{Scheme: "file", Path: target}
		copy, err := engine.NewBackup(address.String())
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, copy.Finish()) }()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := copy.Step(256)
			if err != nil || !more {
				return err
			}
		}
	})
}

// verifyDatabase 从干净快照检查物理完整性、外键和每条受主密钥保护的记录。
func verifyDatabase(ctx context.Context, filename, authDir string, key []byte) (result error) {
	db, err := readDatabase(filename, true)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, db.Close()) }()
	var check string
	if err = db.QueryRowContext(ctx, "PRAGMA integrity_check(1)").Scan(&check); err != nil {
		return err
	}
	if check != "ok" {
		return fmt.Errorf("SQLite 完整性校验失败")
	}
	var broken bool
	if err = db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_foreign_key_check)").Scan(&broken); err != nil {
		return err
	}
	if broken {
		return fmt.Errorf("SQLite 存在无效外键")
	}
	if err = store.VerifyHistory(ctx, db); err != nil {
		return err
	}
	if _, err = auth.ValidateSetup(ctx, db, authDir); err != nil {
		return err
	}
	if err = verifySecrets(ctx, db, key, false); err != nil {
		return err
	}
	return verifySecrets(ctx, db, key, true)
}

func verifySecrets(ctx context.Context, db *sql.DB, key []byte, devices bool) (result error) {
	// 先在 SQLite 内检查容量，只把符合写入端上限的密文送入 Go 内存。
	query := "SELECT id, CASE WHEN length(ciphertext)<=16400 THEN ciphertext END, CASE WHEN length(nonce)=12 THEN nonce END FROM secrets"
	if devices {
		query = "SELECT id,CASE WHEN length(push_token)<=8192 THEN push_token END FROM devices WHERE push_token IS NOT NULL"
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, rows.Close()) }()
	for rows.Next() {
		var id string
		var record struct{ Ciphertext, Nonce []byte }
		if devices {
			var encoded []byte
			if err := rows.Scan(&id, &encoded); err != nil {
				return err
			}
			if err := json.Unmarshal(encoded, &record); err != nil {
				return fmt.Errorf("设备加密记录格式无效")
			}
		} else if err := rows.Scan(&id, &record.Ciphertext, &record.Nonce); err != nil {
			return err
		}
		plain, err := secure.Open(key, record.Ciphertext, record.Nonce, id)
		clear(plain)
		if err != nil {
			return fmt.Errorf("主密钥无法验证现有加密记录")
		}
	}
	return rows.Err()
}
