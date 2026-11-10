package provision

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestClockUsesRequestMidpointAndWarnsAtTenMinutes(t *testing.T) {
	reference := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		skew        time.Duration
		age         string
		status      int
		missingDate bool
		want        string
	}{
		{"准确", 0, "", 200, false, ""},
		{"容差内", 599 * time.Second, "", 200, false, ""},
		{"快十分钟", 600 * time.Second, "", 200, false, "校准"},
		{"慢十分钟", -600 * time.Second, "", 200, false, "校准"},
		{"共享缓存", time.Hour, strconv.Itoa(3600), 200, false, ""},
		{"错误缓存", 0, "-1", 200, false, "缓存"},
		{"未知时间", 0, "", 200, true, "Date"},
		{"不跟随跳转", 0, "", 302, false, "302"},
		{"不可用", 0, "", 503, false, "503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead || r.Header.Get("Authorization") != "" || r.URL.Path != "/" {
					t.Error("校验请求意外携带凭据、正文或跳转")
				}
				w.Header().Set("Date", reference.Format(http.TimeFormat))
				if tc.missingDate {
					w.Header()["Date"] = nil
				}
				if tc.age != "" {
					w.Header().Set("Age", tc.age)
				}
				if tc.status == 302 {
					w.Header().Set("Location", "/unexpected")
				}
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			current := reference.Add(tc.skew - time.Second)
			now := func() time.Time { value := current; current = current.Add(2 * time.Second); return value }
			err := checkClock(context.Background(), server.URL, server.Client().Transport, now)
			if (err == nil) != (tc.want == "") || err != nil && !strings.Contains(err.Error(), tc.want) {
				t.Fatal("时钟告警错误", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := checkClock(ctx, "https://clock.invalid", nil, time.Now); err == nil {
		t.Fatal("取消检查仍报告时钟正常")
	}
}
