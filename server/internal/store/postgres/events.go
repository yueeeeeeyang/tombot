package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Event 是用户内部有序事件；序号只表示当前用户的已提交顺序。
type Event struct {
	Sequence   int64           `json:"sequence"`
	Kind       string          `json:"kind"`
	ResourceID string          `json:"resource_id"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"created_at"`
}

// AppendEvent 在业务事务内追加事件，用户头锁保持到提交，避免晚提交事件被游标越过。
func AppendEvent(ctx context.Context, transaction *sql.Tx, userID, kind, resourceID string, payload json.RawMessage) (int64, error) {
	var object map[string]json.RawMessage
	if transaction == nil || strings.TrimSpace(kind) == "" || len(kind) > 120 || resourceID == "" || len(payload) > 1<<20 || json.Unmarshal(payload, &object) != nil || object == nil {
		return 0, errors.New("事件参数无效，载荷必须为有界 JSON 对象")
	}
	var sequence int64
	if err := transaction.QueryRowContext(ctx, `INSERT INTO tombot.user_event_heads(user_id, sequence) VALUES ($1,1)
	    ON CONFLICT (user_id) DO UPDATE SET sequence = tombot.user_event_heads.sequence + 1 RETURNING sequence`, userID).Scan(&sequence); err != nil {
		return 0, safeDatabaseError("分配用户事件序号失败", err)
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO tombot.user_events(user_id,sequence,kind,resource_id,payload) VALUES ($1,$2,$3,$4,$5::jsonb)`, userID, sequence, kind, resourceID, string(payload)); err != nil {
		return 0, safeDatabaseError("保存用户事件失败", err)
	}
	return sequence, nil
}

// ReadEvents 在当前用户事务中有界读取事件；查询在数据库内隔离用户，失败不返回空列表。
func ReadEvents(ctx context.Context, transaction *sql.Tx, userID string, after int64, limit int) (events []Event, result error) {
	if transaction == nil || after < 0 || limit < 1 || limit > 500 {
		return nil, errors.New("事件分页参数无效")
	}
	rows, err := transaction.QueryContext(ctx, `SELECT sequence,kind,resource_id,payload,created_at
	    FROM tombot.user_events WHERE user_id=$1 AND sequence>$2 ORDER BY sequence LIMIT $3`, userID, after, limit)
	if err != nil {
		return nil, safeDatabaseError("读取用户事件失败", err)
	}
	defer func() { result = errors.Join(result, rows.Close()) }()
	events = make([]Event, 0, limit)
	for rows.Next() {
		var event Event
		if err = rows.Scan(&event.Sequence, &event.Kind, &event.ResourceID, &event.Payload, &event.CreatedAt); err != nil {
			return nil, safeDatabaseError("解析用户事件失败", err)
		}
		events = append(events, event)
	}
	return events, safeDatabaseError("遍历用户事件失败", rows.Err())
}
