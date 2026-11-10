package auth

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit"
)

func requireOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	requireOK(t, db.QueryRow(query, args...).Scan(&n))
	return n
}

func TestSetupAuditCrashBoundariesAndLegacyFile(t *testing.T) {
	db := testkit.Database(t)
	dir := t.TempDir()
	a, err := New(db, dir)
	requireOK(t, err)
	code, err := a.IssueSetupCode()
	requireOK(t, err)
	password := testkit.Random(t)
	_, err = db.Exec("CREATE TRIGGER reject_setup BEFORE INSERT ON audit_log WHEN NEW.action='auth.setup' BEGIN SELECT RAISE(ABORT,'test'); END")
	requireOK(t, err)
	if a.Setup(password, code) == nil || a.Initialized() {
		t.Fatal("审计失败时激活了身份")
	}
	if n := countRows(t, db, "SELECT count(*) FROM system_state WHERE key='auth.setup'"); n != 0 {
		t.Fatal("初始化标记未回滚")
	}
	if _, err = os.Stat(filepath.Join(dir, "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("未提交凭据已发布")
	}
	restored, err := New(db, dir)
	requireOK(t, err)
	if restored.Initialized() {
		t.Fatal("启动时恢复了未提交的凭据")
	}
	if _, err = os.Stat(filepath.Join(dir, pendingName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("未清理未提交的凭据")
	}
	_, err = db.Exec("DROP TRIGGER reject_setup")
	requireOK(t, err)
	code, err = restored.IssueSetupCode()
	requireOK(t, err)
	requireOK(t, restored.Setup(password, code))
	// 复现数据库已提交、凭据还没改名时的持久状态；只读快照检查不得提前发布。
	final := filepath.Join(dir, "auth.json")
	staged := filepath.Join(dir, pendingName)
	requireOK(t, os.Rename(final, staged))
	active, err := ValidateSetup(t.Context(), db, dir)
	requireOK(t, err)
	if !active {
		t.Fatal("已提交初始化未识别")
	}
	if _, err = os.Stat(final); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("备份验证写入了原实例")
	}
	restored, err = New(db, dir)
	requireOK(t, err)
	if code, err := restored.IssueSetupCode(); err != nil || code != "" {
		t.Fatal("恢复后重新签发初始化码")
	}
	if n := countRows(t, db, "SELECT count(*) FROM audit_log WHERE action='auth.setup'"); n != 1 {
		t.Fatal("初始化审计未恰好记录一次")
	}
	_, err = restored.Login(t.Context(), password, "设备", "macos", nil)
	requireOK(t, err)
	requireOK(t, os.Remove(final))
	if _, err = New(db, dir); err == nil {
		t.Fatal("缺少已提交凭据时允许重新初始化")
	}
	oldDB := testkit.Database(t)
	hash, err := secure.HashPassword(password)
	requireOK(t, err)
	body, err := json.Marshal(credentials{hash})
	requireOK(t, err)
	requireOK(t, os.WriteFile(final, body, 0600))
	legacy, err := New(oldDB, dir)
	requireOK(t, err)
	if !legacy.Initialized() {
		t.Fatal("既有认证文件无法加载")
	}
	if countRows(t, oldDB, "SELECT count(*) FROM audit_log") != 0 {
		t.Fatal("为旧数据虚构初始化记录")
	}
}

func TestRegistrationAndRevocationRequireAuditCommit(t *testing.T) {
	db := testkit.Database(t)
	dir := t.TempDir()
	a, err := New(db, dir)
	requireOK(t, err)
	code, err := a.IssueSetupCode()
	requireOK(t, err)
	password := testkit.Random(t)
	requireOK(t, a.Setup(password, code))
	key, err := secure.Random(32)
	requireOK(t, err)
	defer clear(key)
	push := &PushRegistration{testkit.Random(t), key}
	deviceName := testkit.Random(t)
	ctx := t.Context()
	if _, err = a.Login(ctx, password, deviceName, "linux", &PushRegistration{strings.Repeat("x", 4097), key}); err == nil {
		t.Fatal("接受过长推送凭据")
	}
	_, err = db.Exec("CREATE TRIGGER reject_audit BEFORE INSERT ON audit_log BEGIN SELECT RAISE(ABORT,'test'); END")
	requireOK(t, err)
	if _, err = a.Login(ctx, password, deviceName, "linux", push); err == nil {
		t.Fatal("审计失败时返回授权")
	}
	if countRows(t, db, "SELECT count(*) FROM devices") != 0 {
		t.Fatal("登录失败后留下设备")
	}
	_, err = db.Exec("DROP TRIGGER reject_audit")
	requireOK(t, err)
	if _, err = a.Login(ctx, testkit.Random(t), deviceName, "linux", nil); err == nil {
		t.Fatal("错误口令被接受")
	}
	session, err := a.Login(ctx, password, deviceName, "linux", push)
	requireOK(t, err)
	var before, after []byte
	requireOK(t, db.QueryRow("SELECT push_token FROM devices WHERE id=?", session.DeviceID).Scan(&before))
	var sealed struct{ Ciphertext, Nonce []byte }
	requireOK(t, json.Unmarshal(before, &sealed))
	plaintext, err := secure.Open(key, sealed.Ciphertext, sealed.Nonce, session.DeviceID)
	requireOK(t, err)
	if string(plaintext) != push.Token {
		t.Fatal("登录未保存原推送凭据")
	}
	clear(plaintext)
	_, err = db.Exec("CREATE TRIGGER reject_audit BEFORE INSERT ON audit_log BEGIN SELECT RAISE(ABORT,'test'); END")
	requireOK(t, err)
	if a.UpdatePushToken(ctx, session.DeviceID, testkit.Random(t), key) == nil || a.Revoke(ctx, session.DeviceID, session.DeviceID) == nil {
		t.Fatal("审计失败时设备变更成功")
	}
	requireOK(t, db.QueryRow("SELECT push_token FROM devices WHERE id=?", session.DeviceID).Scan(&after))
	if !bytes.Equal(before, after) {
		t.Fatal("推送凭据未回滚")
	}
	_, err = a.Authenticate(ctx, session.DeviceToken)
	requireOK(t, err)
	_, err = db.Exec("DROP TRIGGER reject_audit")
	requireOK(t, err)
	requireOK(t, a.UpdatePushToken(ctx, session.DeviceID, testkit.Random(t), key))
	actorID := ids.New()
	requireOK(t, a.Revoke(ctx, session.DeviceID, actorID))
	requireOK(t, a.Revoke(ctx, session.DeviceID, actorID))
	if a.UpdatePushToken(ctx, session.DeviceID, testkit.Random(t), key) == nil {
		t.Fatal("撤销后仍允许更新推送凭据")
	}
	if countRows(t, db, "SELECT count(*) FROM audit_log WHERE actor=? AND target=? AND action='auth.device.revoke'", "device:"+actorID, session.DeviceID) != 1 {
		t.Fatal("撤销审计重复或归属错误")
	}
	var summary string
	requireOK(t, db.QueryRow("SELECT group_concat(actor||action||target||detail_json) FROM audit_log").Scan(&summary))
	for _, field := range []string{code, password, deviceName, session.DeviceToken, push.Token, secure.Hash(session.DeviceToken)} {
		if strings.Contains(summary, field) {
			t.Fatal("审计泄露输入或密钥材料")
		}
	}
	for _, action := range []string{"auth.setup", "auth.login.denied", "auth.login", "auth.device.push", "auth.device.revoke"} {
		if !strings.Contains(summary, action) {
			t.Fatal("审计遗漏", action)
		}
	}
}
