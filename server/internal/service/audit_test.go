package service

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	safelog "io.yueyang/tom/internal/infra/log"
	"io.yueyang/tom/internal/store"
	"io.yueyang/tom/internal/testkit"
)

func TestAuditPaginationRedactionAndOldRows(t *testing.T) {
	db := testkit.Database(t)
	ctx := t.Context()
	audit := NewAuditLog(db)
	token := "tomd_" + testkit.Random(t)
	private := testkit.Random(t)
	raw, err := json.Marshal(map[string]any{"nested": []any{map[string]any{"api_key": private, "note": "Bearer " + private}}, "path": "out/" + token, "cost": 9007199254740993})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old", "mid", "new"} {
		if _, err = db.Exec("INSERT INTO audit_log(id,actor,action,target,detail_json,ip,created_at) VALUES(?,?,?,?,?,?,?)", id, "device:"+token, "audit.test", token, string(raw), private, 0); err != nil {
			t.Fatal(err)
		}
	}
	page, err := audit.List(ctx, "", 2)
	if err != nil || len(page.Records) != 2 || page.NextCursor == "" {
		t.Fatal("首屏错误", err)
	}
	if page.Records[0].ID != "new" || page.Records[1].ID != "mid" {
		t.Fatal("同一时刻分页次序错误")
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), private) || strings.Contains(string(encoded), token) || !strings.Contains(string(encoded), "9007199254740993") {
		t.Fatal("历史记录脱敏或整数精度错误")
	}
	err = store.Transact(ctx, db, func(tx *sql.Tx) error {
		return store.Audit(ctx, tx, "user", "audit.test", "", map[string]any{"password": private, "note": "Bearer " + private})
	})
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err = db.QueryRow("SELECT detail_json FROM audit_log ORDER BY rowid DESC LIMIT 1").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, private) {
		t.Fatal("新审计明文落库")
	}
	second, err := audit.List(ctx, page.NextCursor, 2)
	if err != nil || len(second.Records) != 1 || second.Records[0].ID != "old" || second.NextCursor != "" {
		t.Fatal("并发追加导致分页漂移", err)
	}
	for _, cursor := range []string{"0", "-1", "+1", "01", "9223372036854775808", "x"} {
		if _, err = audit.List(ctx, cursor, 1); err == nil {
			t.Fatal("无效游标被接受")
		}
	}
	for _, limit := range []int{0, 201} {
		if _, err = audit.List(ctx, "", limit); err == nil {
			t.Fatal("无效容量被接受")
		}
	}
	for _, bad := range []string{"invalid", strings.Repeat(" ", safelog.MaxAuditDetails+1)} {
		if _, err = db.Exec("UPDATE audit_log SET detail_json=? WHERE id='old'", bad); err != nil {
			t.Fatal(err)
		}
		if _, err = audit.List(ctx, page.NextCursor, 2); err == nil {
			t.Fatal("损坏审计被静默跳过")
		}
	}
}
