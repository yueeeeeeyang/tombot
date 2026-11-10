package service

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/store"
	"io.yueyang/tom/internal/usage"
)

type Usage struct{ db *sql.DB }

func NewUsage(db *sql.DB) *Usage { return &Usage{db: db} }

type UsageQuery struct {
	From, To        time.Time
	GroupBy, Cursor string
	Limit           int
}

// Report 校验查询条件。用量事件已删除，分组和汇总保持为空，回合费用仍在 runs 上。
func (s *Usage) Report(ctx context.Context, query UsageQuery) (domain.UsageReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	now := time.Now().UTC()
	if query.From.IsZero() && query.To.IsZero() {
		query.From, query.To = usage.MonthBounds(now)
	}
	if query.GroupBy == "" {
		query.GroupBy = "day"
	}
	if query.Limit == 0 {
		query.Limit = 50
	}
	var report domain.UsageReport
	if query.From.IsZero() || query.To.IsZero() || query.From.Year() < 1970 || query.To.Year() > 2200 || !query.From.Before(query.To) || query.To.Sub(query.From) > 366*24*time.Hour || query.Limit < 1 || query.Limit > 200 || len(query.Cursor) > 128 || strings.ContainsRune(query.Cursor, 0) {
		return report, domain.NewError(400, "USAGE_QUERY_INVALID", "Use a UTC date range of at most 366 days, a valid cursor, and a limit from 1 to 200.")
	}
	switch query.GroupBy {
	case "bot", "model", "day":
	default:
		return report, domain.NewError(400, "USAGE_QUERY_INVALID", "Group usage by bot, model, or day.")
	}
	report = domain.UsageReport{From: query.From.UTC(), To: query.To.UTC(), GroupBy: query.GroupBy, Groups: []domain.UsageGroup{}}
	err := store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		report.Total, err = usage.Totals(ctx, tx, query.From, query.To)
		if err != nil {
			return err
		}
		report.Monthly, err = usage.Monthly(ctx, tx, now)
		return err
	})
	return report, err
}
