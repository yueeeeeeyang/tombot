// Package scheduler 负责例程时间规则，业务持久化和执行仍由服务与 Agent 运行时承担。
package scheduler

import (
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
	"io.yueyang/tom/internal/domain"
)

// Next 返回目标时区中严格晚于 after 的下一次触发；只接受五字段 cron，拒绝无下次时间的规则。
func Next(expression, timezone string, after time.Time) (time.Time, error) {
	if len(expression) > 256 || len(timezone) > 100 || len(strings.Fields(expression)) != 5 || strings.Contains(expression, "=") || strings.ContainsAny(timezone, " \t\r\n=") || timezone == "" || timezone == "Local" {
		return time.Time{}, domain.NewError(400, "ROUTINE_SCHEDULE_INVALID", "Use a five-field cron expression and an explicit IANA time zone.")
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return time.Time{}, domain.NewError(400, "ROUTINE_SCHEDULE_INVALID", "The time zone is not available.")
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, err := parser.Parse("CRON_TZ=" + timezone + " " + expression)
	if err != nil {
		return time.Time{}, domain.NewError(400, "ROUTINE_SCHEDULE_INVALID", "The cron expression is invalid.")
	}
	next := schedule.Next(after)
	if next.IsZero() {
		return time.Time{}, domain.NewError(400, "ROUTINE_SCHEDULE_INVALID", "The cron expression has no upcoming occurrence.")
	}
	return next.UTC(), nil
}
