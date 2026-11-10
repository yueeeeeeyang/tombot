package httpapi

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/testkit"
)

func TestUsageHTTPAuthenticationRangeAndBudgetConflict(t *testing.T) {
	s, code := fixture(t, true)
	password := testkit.Random(t)
	if err := s.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := s.Auth.Login(context.Background(), password, "用量测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	handler := s.Handler()
	if response := request(t, handler, "GET", "/api/v1/usage", nil, nil); response.Code != 401 {
		t.Fatal(response.Code)
	}
	response := request(t, handler, "GET", "/api/v1/usage?group_by=bot&limit=1", nil, headers)
	var report domain.UsageReport
	if err = json.Unmarshal(response.Body.Bytes(), &report); err != nil || response.Code != 200 || report.Total.CostMicro != 0 || report.Monthly.Status != "unlimited" || report.Groups == nil {
		t.Fatal(response.Code, err)
	}
	for _, query := range []string{"from=invalid", "from=" + url.QueryEscape(time.Now().Format(time.RFC3339)), "group_by=run", "limit=201", "from=2024-01-01T00:00:00Z&to=2026-01-01T00:00:00Z"} {
		if response = request(t, handler, "GET", "/api/v1/usage?"+query, nil, headers); response.Code != 400 {
			t.Fatal(query, response.Code)
		}
	}
	patch := map[string]any{"monthly_budget": map[string]any{"revision": 0, "limit_micro": 1000}}
	if response = request(t, handler, "PATCH", "/api/v1/settings", patch, headers); response.Code != 200 {
		t.Fatal(response.Code)
	}
	if response = request(t, handler, "PATCH", "/api/v1/settings", patch, headers); response.Code != 409 {
		t.Fatal("旧预算版本未冲突", response.Code)
	}
	response = request(t, handler, "GET", "/api/v1/usage", nil, headers)
	if err = json.Unmarshal(response.Body.Bytes(), &report); err != nil || report.Monthly.Budget.Revision != 1 || *report.Monthly.Budget.LimitMicro != 1000 {
		t.Fatal(err, report)
	}
}
