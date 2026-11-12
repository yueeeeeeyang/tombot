package bus

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const Retention = 2000

// Event 是可续传事件；Payload 必须在发布前完成脱敏。
type Event struct {
	ID             int64           `json:"id"`
	Kind           string          `json:"kind"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Payload        json.RawMessage `json:"payload"`
}

// Bus 以数据库为权威；通知只负责唤醒，不使用会丢消息的每客户端缓冲。
type Bus struct {
	db   *sql.DB
	mu   sync.Mutex
	wake chan struct{}
}

func New(db *sql.DB) *Bus { return &Bus{db: db, wake: make(chan struct{})} }

// Publish 持久化并裁剪后唤醒订阅者；事务失败不产生通知。
func (b *Bus) Publish(ctx context.Context, kind, conversationID string, payload any) (id int64, err error) {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			rollbackErr := tx.Rollback()
			if !errors.Is(rollbackErr, sql.ErrTxDone) {
				err = errors.Join(err, rollbackErr)
			}
		}
	}()
	id, err = Append(ctx, tx, kind, conversationID, payload)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	b.Notify()
	return id, nil
}

// Append 将事件加入调用方事务；提交成功后必须 Notify，避免业务与事件状态分离。
func Append(ctx context.Context, tx *sql.Tx, kind, conversationID string, payload any) (id int64, err error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, "INSERT INTO events(kind,conversation_id,payload_json,created_at) VALUES(?,?,?,?)", kind, conversationID, string(encoded), time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	id, err = result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM events WHERE id<=?", id-Retention); err != nil {
		return 0, err
	}
	return id, nil
}

// Notify 唤醒等待持久事件的消费者，仅能在事务提交之后调用。
func (b *Bus) Notify() { b.mu.Lock(); close(b.wake); b.wake = make(chan struct{}); b.mu.Unlock() }

// Read 返回游标后的一页；越界时要求重新同步。调用方先取通知通道再读取，避免丢失唤醒。
func (b *Bus) Read(ctx context.Context, after int64) ([]Event, bool, error) {
	var first, last sql.NullInt64
	if err := b.db.QueryRowContext(ctx, "SELECT MIN(id),MAX(id) FROM events").Scan(&first, &last); err != nil {
		return nil, false, err
	}
	if after > last.Int64 || (after > 0 && first.Valid && after < first.Int64-1) {
		return nil, true, nil
	}
	rows, err := b.db.QueryContext(ctx, "SELECT id,kind,conversation_id,payload_json FROM events WHERE id>? ORDER BY id LIMIT 200", after)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var event Event
		var payload string
		if err = rows.Scan(&event.ID, &event.Kind, &event.ConversationID, &payload); err != nil {
			return nil, false, err
		}
		event.Payload = json.RawMessage(payload)
		events = append(events, event)
	}
	return events, false, rows.Err()
}

// Wake 返回下一次提交后关闭的通道；没有订阅者登记或泄漏问题。
func (b *Bus) Wake() <-chan struct{} { b.mu.Lock(); defer b.mu.Unlock(); return b.wake }
