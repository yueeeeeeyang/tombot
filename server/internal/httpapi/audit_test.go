package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestAuditEndpointsRequireDeviceAndCompleteExport(t *testing.T) {
	s, code := fixture(t, true)
	handler := s.Handler()
	for _, path := range []string{"/api/v1/audit", "/api/v1/audit/export"} {
		if w := request(t, handler, "GET", path, nil, nil); w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
	password := testkit.Random(t)
	if err := s.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := s.Auth.Login(t.Context(), password, "测试", "linux", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	for range 207 {
		if err = s.Audit.BeginExport(t.Context(), "user"); err != nil {
			t.Fatal(err)
		}
	}
	w := request(t, handler, "GET", "/api/v1/audit?limit=1", nil, h)
	var page service.AuditPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Records) != 1 || page.NextCursor == "" {
		t.Fatal("分页响应错误")
	}
	for _, query := range []string{"cursor=invalid", "limit=0", "limit=201"} {
		if request(t, handler, "GET", "/api/v1/audit?"+query, nil, h).Code != 400 {
			t.Fatal("错误分页未被拒绝")
		}
	}
	w = request(t, handler, "GET", "/api/v1/audit/export", nil, h)
	var records []service.AuditEntry
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") || json.Unmarshal(w.Body.Bytes(), &records) != nil || len(records) < 210 {
		t.Fatal("未导出全部分页")
	}
	seen := map[string]bool{}
	for _, row := range records {
		if seen[row.ID] {
			t.Fatal("导出重复记录")
		}
		seen[row.ID] = true
	}
	if records[0].Actor != "device:"+session.DeviceID || records[0].Action != "audit.export" {
		t.Fatal("导出未登记发起设备")
	}
	for _, value := range []string{session.DeviceToken, password, code} {
		if strings.Contains(w.Body.String(), value) {
			t.Fatal("导出含凭据")
		}
	}
	// 首批发送期间撤销，第二批开始前必须中断，客户端不能将前缀解析为完整文件。
	r := httptest.NewRequest("GET", "/api/v1/audit/export", nil)
	r.Header.Set("Authorization", h["Authorization"])
	gate := &revokingAuditWriter{ResponseRecorder: httptest.NewRecorder(), revoke: func() {
		if err := s.Auth.Revoke(context.Background(), session.DeviceID, session.DeviceID); err != nil {
			t.Fatal(err)
		}
	}}
	func() {
		defer func() {
			if got := recover(); got != http.ErrAbortHandler {
				t.Fatalf("未中断流 %v", got)
			}
		}()
		handler.ServeHTTP(gate, r)
	}()
	if json.Valid(gate.Body.Bytes()) {
		t.Fatal("导出伪装成完整 JSON")
	}
	if request(t, handler, "GET", "/api/v1/audit/export", nil, h).Code != 401 {
		t.Fatal("撤销后仍可导出")
	}
}

type revokingAuditWriter struct {
	*httptest.ResponseRecorder
	revoke func()
}

func (w *revokingAuditWriter) Write(b []byte) (int, error) {
	if w.revoke != nil {
		f := w.revoke
		w.revoke = nil
		f()
	}
	return w.ResponseRecorder.Write(b)
}
