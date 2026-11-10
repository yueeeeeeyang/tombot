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

// Report 仅聚合实际调用事件，父子运行的累计费用不参与汇总；分页与总数共享事务快照。
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
	key, name := "", ""
	switch query.GroupBy {
	case "bot":
		key = "COALESCE(u.bot_id,'system')"
		name = "COALESCE(b.name,CASE WHEN u.bot_id IS NULL THEN '系统' ELSE '已删除的 Bot' END)"
	case "model":
		key = "u.model_id"
		name = "COALESCE(m.display_name,CASE WHEN u.model_id LIKE 'probe:%' THEN '未配置模型的连通性测试' ELSE '已删除的模型' END)"
	case "day":
		key = "strftime('%Y-%m-%d',u.created_at/1000,'unixepoch')"
		name = key
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
		if err != nil {
			return err
		}
		// 分组表达式来自固定白名单；外部日期、游标与页大小始终绑定参数。
		rows, err := tx.QueryContext(ctx, `SELECT `+key+`,`+name+`,COUNT(*),SUM(u.input_tokens),SUM(u.output_tokens),SUM(u.cache_read_tokens),SUM(u.cost_micro),SUM(CASE WHEN u.is_estimated!=0 THEN 1 ELSE 0 END) FROM usage_events u LEFT JOIN bots b ON b.id=u.bot_id LEFT JOIN models m ON m.id=u.model_id WHERE u.created_at>=? AND u.created_at<? AND (?='' OR `+key+`>?) GROUP BY `+key+` ORDER BY `+key+` LIMIT ?`, query.From.UnixMilli(), query.To.UnixMilli(), query.Cursor, query.Cursor, query.Limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var group domain.UsageGroup
			if err = rows.Scan(&group.ID, &group.Name, &group.Calls, &group.InputTokens, &group.OutputTokens, &group.CacheReadTokens, &group.CostMicro, &group.EstimatedCalls); err != nil {
				return err
			}
			if len(report.Groups) == query.Limit {
				report.NextCursor = report.Groups[len(report.Groups)-1].ID
				break
			}
			report.Groups = append(report.Groups, group)
		}
		return rows.Err()
	})
	return report, err
}
