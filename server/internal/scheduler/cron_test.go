package scheduler

import (
	"testing"
	"time"
)

func TestNextTimezoneDSTAndBoundaries(t *testing.T) {
	for _, test := range []struct{ expression, zone, after, want string }{
		{"0 8 * * *", "Asia/Shanghai", "2026-11-06T00:00:00Z", "2026-11-07T00:00:00Z"},
		{"30 2 * * *", "America/New_York", "2026-03-08T05:00:00Z", "2026-03-09T06:30:00Z"},
		{"30 1 * * *", "America/New_York", "2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z"},
		{"0 0 29 2 *", "UTC", "2027-03-01T00:00:00Z", "2028-02-29T00:00:00Z"},
	} {
		t.Run(test.zone+test.after, func(t *testing.T) {
			after, err := time.Parse(time.RFC3339, test.after)
			if err != nil {
				t.Fatal(err)
			}
			next, err := Next(test.expression, test.zone, after)
			if err != nil || next.Format(time.RFC3339) != test.want {
				t.Fatal(next, err, "预期", test.want)
			}
		})
	}
	for _, test := range [][2]string{{"* * * * * *", "UTC"}, {"@every 1s", "UTC"}, {"TZ=UTC * * * * *", "UTC"}, {"0 0 30 2 *", "UTC"}, {"* * * * *", "Local"}, {"* * * * *", "bad/zone"}, {"* * * * *", "UTC\n"}, {"60 * * * *", "UTC"}} {
		if _, err := Next(test[0], test[1], time.Now()); err == nil {
			t.Fatal("无效计划被接受", test)
		}
	}
}
