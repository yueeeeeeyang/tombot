package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"io.yueyang/tom/internal/domain"
	safelog "io.yueyang/tom/internal/infra/log"
	"io.yueyang/tom/internal/store"
)

type AuditLog struct{ db *sql.DB }

func NewAuditLog(db *sql.DB) *AuditLog { return &AuditLog{db: db} }

type AuditEntry struct {
	ID        string          `json:"id"`
	Actor     string          `json:"actor"`
	Action    string          `json:"action"`
	Target    string          `json:"target,omitempty"`
	Details   json.RawMessage `json:"details"`
	CreatedAt time.Time       `json:"created_at"`
}

type AuditPage struct {
	Records    []AuditEntry `json:"records"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// List 按追加顺序倒序分页。rowid 范围能直接定位下一页，时钟回拨和并发追加不改变已开始的浏览。
func (s *AuditLog) List(ctx context.Context, cursor string, limit int) (page AuditPage, result error) {
	if limit < 1 || limit > 200 {
		return page, domain.NewError(400, "INVALID_LIMIT", "Limit must be between 1 and 200.")
	}
	query := `SELECT rowid,
  CASE WHEN length(CAST(id AS BLOB))<=128 THEN id END,
  CASE WHEN length(CAST(actor AS BLOB))<=256 THEN actor END,
  CASE WHEN length(CAST(action AS BLOB))<=128 THEN action END,
  CASE WHEN length(CAST(COALESCE(target,'') AS BLOB))<=4096 THEN COALESCE(target,'') END,
  CASE WHEN length(CAST(detail_json AS BLOB))<=? THEN detail_json END,created_at FROM audit_log`
	args := []any{safelog.MaxAuditDetails}
	if cursor != "" {
		before, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || before <= 0 || strconv.FormatInt(before, 10) != cursor {
			return page, domain.NewError(400, "AUDIT_CURSOR_INVALID", "Provide a valid audit cursor.")
		}
		query += " WHERE rowid<?"
		args = append(args, before)
	}
	rows, err := s.db.QueryContext(ctx, query+" ORDER BY rowid DESC LIMIT ?", append(args, limit+1)...)
	if err != nil {
		return page, err
	}
	defer func() { result = errors.Join(result, rows.Close()) }()
	page.Records = make([]AuditEntry, 0, limit)
	var last int64
	for rows.Next() {
		if len(page.Records) == limit {
			page.NextCursor = strconv.FormatInt(last, 10)
			break
		}
		var item AuditEntry
		var stamp int64
		var raw []byte
		if err = rows.Scan(&last, &item.ID, &item.Actor, &item.Action, &item.Target, &raw, &stamp); err != nil {
			return page, err
		}
		if item.Details, err = safelog.SafeDetails(raw); err != nil {
			return page, fmt.Errorf("审计记录 %s 的详情无效：%w", safelog.SafeText(item.ID), err)
		}
		item.ID = safelog.SafeText(item.ID)
		item.Actor = safelog.SafeText(item.Actor)
		item.Action = safelog.SafeText(item.Action)
		item.Target = safelog.SafeText(item.Target)
		item.CreatedAt = time.UnixMilli(stamp).UTC()
		page.Records = append(page.Records, item)
	}
	return page, rows.Err()
}

// BeginExport 在发送第一条记录前记录导出请求，故障时不输出部分成功结果。
func (s *AuditLog) BeginExport(ctx context.Context, actor string) error {
	return store.Transact(ctx, s.db, func(tx *sql.Tx) error { return store.Audit(ctx, tx, actor, "audit.export", "audit", struct{}{}) })
}
