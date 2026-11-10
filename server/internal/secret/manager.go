package secret

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"io.yueyang/tom/internal/domain"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store"
)

type Metadata struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Hint      string     `json:"hint"`
	CreatedAt time.Time  `json:"created_at"`
	RotatedAt *time.Time `json:"rotated_at"`
}

// Manager 只提供元数据查询与受控执行期解密，明文不能出现在读取接口。
type Manager struct {
	db  *sql.DB
	key []byte
}

func New(db *sql.DB, key []byte) *Manager { return &Manager{db: db, key: key} }
func validate(name, value string) error {
	if strings.TrimSpace(name) == "" || len(name) > 120 || len(value) < 8 || len(value) > 16<<10 || !utf8.ValidString(value) {
		return domain.NewError(400, "SECRET_INVALID", "Provide a name and a secret between 8 and 16384 bytes.")
	}
	return nil
}
func hint(value string) string {
	characters := []rune(value)
	if len(characters) <= 4 {
		return "••••"
	}
	return "••••" + string(characters[len(characters)-4:])
}

// Create 将新值加密后写入；仅返回名称与末尾提示。
func (m *Manager) Create(ctx context.Context, name, value, actor string) (Metadata, error) {
	var record Metadata
	err := store.Transact(ctx, m.db, func(tx *sql.Tx) error {
		var err error
		record, err = m.CreateInTransaction(ctx, tx, name, value, actor)
		return err
	})
	return record, err
}

// CreateInTransaction 与引用密钥的业务配置原子提交，失败由调用方回滚整个事务。
func (m *Manager) CreateInTransaction(ctx context.Context, tx *sql.Tx, name, value, actor string) (Metadata, error) {
	name = strings.TrimSpace(name)
	if err := validate(name, value); err != nil {
		return Metadata{}, err
	}
	record := Metadata{ID: ids.New(), Name: name, Hint: hint(value), CreatedAt: time.Now().UTC()}
	plain := []byte(value)
	defer clear(plain)
	ciphertext, nonce, err := secure.Seal(m.key, plain, record.ID)
	if err != nil {
		return record, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO secrets(id,name,ciphertext,nonce,hint,created_at) VALUES(?,?,?,?,?,?)", record.ID, record.Name, ciphertext, nonce, record.Hint, record.CreatedAt.UnixMilli()); err != nil {
		return record, store.ConstraintError(err)
	}
	return record, store.Audit(ctx, tx, actor, "secret.create", record.ID, map[string]string{"name": name})
}
func (m *Manager) List(ctx context.Context, cursor string, limit int) ([]Metadata, error) {
	rows, err := m.db.QueryContext(ctx, "SELECT id,name,hint,created_at,rotated_at FROM secrets WHERE (?='' OR id<?) ORDER BY id DESC LIMIT ?", cursor, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []Metadata{}
	for rows.Next() {
		var r Metadata
		var created int64
		var rotated sql.NullInt64
		if err = rows.Scan(&r.ID, &r.Name, &r.Hint, &created, &rotated); err != nil {
			return nil, err
		}
		r.CreatedAt = time.UnixMilli(created).UTC()
		if rotated.Valid {
			value := time.UnixMilli(rotated.Int64).UTC()
			r.RotatedAt = &value
		}
		records = append(records, r)
	}
	return records, rows.Err()
}
func (m *Manager) Rotate(ctx context.Context, id, value, actor string) error {
	return store.Transact(ctx, m.db, func(tx *sql.Tx) error {
		return m.RotateInTransaction(ctx, tx, id, value, actor)
	})
}

// RotateInTransaction 保证密钥轮换与依赖它的配置同时生效，不在事务外留下新密钥。
func (m *Manager) RotateInTransaction(ctx context.Context, tx *sql.Tx, id, value, actor string) error {
	if err := validate("rotation", value); err != nil {
		return err
	}
	plain := []byte(value)
	defer clear(plain)
	ciphertext, nonce, err := secure.Seal(m.key, plain, id)
	if err != nil {
		return err
	}
	if err := store.RequireChanged(tx.ExecContext(ctx, "UPDATE secrets SET ciphertext=?,nonce=?,hint=?,rotated_at=? WHERE id=?", ciphertext, nonce, hint(value), time.Now().UnixMilli(), id)); err != nil {
		return err
	}
	return store.Audit(ctx, tx, actor, "secret.rotate", id, map[string]string{})
}
func (m *Manager) Delete(ctx context.Context, id, actor string) error {
	return store.Transact(ctx, m.db, func(tx *sql.Tx) error {
		if err := store.RequireChanged(tx.ExecContext(ctx, "DELETE FROM secrets WHERE id=?", id)); err != nil {
			return err
		}
		return store.Audit(ctx, tx, actor, "secret.delete", id, map[string]string{})
	})
}

// Use 在审计成功后把明文交给回调，用完清零；回调不得保留切片或记录值。
func (m *Manager) Use(ctx context.Context, id, actor, runID string, execute func([]byte) error) error {
	var plaintext []byte
	err := store.Transact(ctx, m.db, func(tx *sql.Tx) error {
		var ciphertext, nonce []byte
		var name string
		if err := tx.QueryRowContext(ctx, "SELECT name,ciphertext,nonce FROM secrets WHERE id=?", id).Scan(&name, &ciphertext, &nonce); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.NewError(404, "SECRET_NOT_FOUND", "The secret does not exist.")
			}
			return err
		}
		var err error
		plaintext, err = secure.Open(m.key, ciphertext, nonce, id)
		if err != nil {
			return domain.NewError(500, "SECRET_DECRYPTION_FAILED", "The secret cannot be decrypted with this master key.")
		}
		return store.Audit(ctx, tx, actor, "secret.use", id, map[string]string{"name": name, "run_id": runID})
	})
	defer clear(plaintext)
	if err != nil {
		return err
	}
	return execute(plaintext)
}
