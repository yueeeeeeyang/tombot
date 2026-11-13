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

// PruneUserEvents 删除保留序号之外的历史事件，并把清理边界推进到实际删除的最大序号。
// 事件只提示客户端重新读取资源，删除历史事件不会丢失状态；保留条数按序号计，
// 与事件追加时的顺序一致，游标是否仍然有效可以直接比较边界，不依赖时钟。
// 单次删除受 batch 限制：积压很多时分多次推进，避免一个事务长时间持有事件头锁；
// 未删完时边界仍停在已删位置，下一轮会再次选中该用户。
func PruneUserEvents(ctx context.Context, transaction *sql.Tx, userID string, retention, batch int) (int64, error) {
	if transaction == nil || userID == "" || retention < 1 || retention > 1_000_000 || batch < 1 || batch > 10_000 {
		return 0, errors.New("事件清理参数无效")
	}
	// 事件头行在追加时被锁定；先加锁读取再推进，避免并发追加产生空洞边界。
	var sequence, prunedThrough int64
	err := transaction.QueryRowContext(ctx, `SELECT sequence,pruned_through FROM tombot.user_event_heads WHERE user_id=$1 FOR UPDATE`, userID).Scan(&sequence, &prunedThrough)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, safeDatabaseError("读取用户事件序号失败", err)
	}
	boundary := sequence - int64(retention)
	if boundary <= prunedThrough {
		return prunedThrough, nil
	}
	// 只删除最早的一批并返回其中最大序号；DELETE 需要读取要删除的行，控制连接已有只读授权。
	var deleted int64
	err = transaction.QueryRowContext(ctx, `WITH pruned AS (
	    DELETE FROM tombot.user_events WHERE user_id=$1 AND sequence IN (
	        SELECT sequence FROM tombot.user_events WHERE user_id=$1 AND sequence<=$2 ORDER BY sequence LIMIT $3)
	    RETURNING sequence)
	    SELECT COALESCE(max(sequence),0) FROM pruned`, userID, boundary, batch).Scan(&deleted)
	if err != nil {
		return 0, safeDatabaseError("清理用户历史事件失败", err)
	}
	if deleted <= prunedThrough {
		return prunedThrough, nil
	}
	if _, err = transaction.ExecContext(ctx, `UPDATE tombot.user_event_heads SET pruned_through=$2 WHERE user_id=$1`, userID, deleted); err != nil {
		return 0, safeDatabaseError("推进用户事件清理边界失败", err)
	}
	return deleted, nil
}

// PrunedThrough 返回该用户已清理到的事件序号；客户端游标低于它必须重新读取快照。
func PrunedThrough(ctx context.Context, transaction *sql.Tx, userID string) (int64, error) {
	if transaction == nil || userID == "" {
		return 0, errors.New("事件清理边界参数无效")
	}
	var boundary int64
	err := transaction.QueryRowContext(ctx, `SELECT pruned_through FROM tombot.user_event_heads WHERE user_id=$1`, userID).Scan(&boundary)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, safeDatabaseError("读取用户事件清理边界失败", err)
	}
	return boundary, nil
}

// PrunableUsers 列出事件条数超过保留值、需要清理的用户，按清理边界与序号排序。
// 分批返回而不是一次扫描全表，避免清理任务随时间线性变长。
func PrunableUsers(ctx context.Context, transaction *sql.Tx, retention int, limit int) ([]string, error) {
	if transaction == nil || retention < 1 || limit < 1 || limit > 10_000 {
		return nil, errors.New("待清理用户查询参数无效")
	}
	rows, err := transaction.QueryContext(ctx, `SELECT user_id FROM tombot.user_event_heads
	    WHERE sequence > $1 AND pruned_through < sequence - $1 ORDER BY user_id LIMIT $2`, retention, limit)
	if err != nil {
		return nil, safeDatabaseError("查询待清理用户事件失败", err)
	}
	defer rows.Close()
	var userIDs []string
	for rows.Next() {
		var userID string
		if err = rows.Scan(&userID); err != nil {
			return nil, safeDatabaseError("解析待清理用户失败", err)
		}
		userIDs = append(userIDs, userID)
	}
	return userIDs, safeDatabaseError("遍历待清理用户失败", rows.Err())
}
