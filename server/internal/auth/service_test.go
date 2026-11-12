package auth

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/testkit"
)

func TestSetupLoginRevokeAndExpiry(t *testing.T) {
	db := testkit.Database(t)
	dir := t.TempDir()
	service, err := New(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	code, err := service.IssueSetupCode()
	if err != nil {
		t.Fatal(err)
	}
	password := testkit.Random(t)
	if err = service.Setup(password, testkit.Random(t)); err == nil {
		t.Fatal("错误初始化码必须拒绝")
	}
	if err = service.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	if err = service.Setup(password, code); err == nil {
		t.Fatal("重复初始化必须拒绝")
	}
	encoded, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), password) {
		t.Fatal("口令明文泄漏")
	}
	reloaded, err := New(db, dir)
	if err != nil || !reloaded.Initialized() {
		t.Fatalf("认证未持久化: %v", err)
	}
	ctx := context.Background()
	session, err := reloaded.Login(ctx, password, "测试设备", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := reloaded.Authenticate(ctx, session.DeviceToken)
	if err != nil || id != session.DeviceID {
		t.Fatalf("登录失败: %v", err)
	}
	var hash string
	if err = db.QueryRow("SELECT token_hash FROM devices WHERE id=?", id).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, session.DeviceToken) {
		t.Fatal("令牌明文泄漏")
	}
	devices, err := reloaded.List(ctx, id, "", 10)
	if err != nil || len(devices) != 1 || !devices[0].IsCurrent {
		t.Fatalf("设备列表错误: %v", err)
	}
	if _, err = db.Exec("UPDATE devices SET expires_at=? WHERE id=?", time.Now().Add(-time.Second).UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
	if _, err = reloaded.Authenticate(ctx, session.DeviceToken); err == nil {
		t.Fatal("过期令牌仍然可用")
	}
	if _, err = db.Exec("UPDATE devices SET expires_at=NULL WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if err = reloaded.Revoke(ctx, id, id); err != nil {
		t.Fatal(err)
	}
	if _, err = reloaded.Authenticate(ctx, session.DeviceToken); err == nil {
		t.Fatal("撤销令牌仍然可用")
	}
}

func TestConcurrentSetupAndLoginBackoff(t *testing.T) {
	service, err := New(testkit.Database(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	code, err := service.IssueSetupCode()
	if err != nil {
		t.Fatal(err)
	}
	password := testkit.Random(t)
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); results <- service.Setup(password, code) }()
	}
	workers.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("并发初始化成功次数: %d", success)
	}
	now := time.Now()
	service.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		if _, err = service.Login(context.Background(), testkit.Random(t), "测试设备", "linux", nil); err == nil {
			t.Fatal("错误口令被接受")
		}
	}
	if _, err = service.Login(context.Background(), password, "测试设备", "linux", nil); err == nil {
		t.Fatal("退避期间仍允许登录")
	}
	now = now.Add(2 * time.Second)
	if _, err = service.Login(context.Background(), password, "测试设备", "linux", nil); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptAuthNeverResets(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(testkit.Database(t), dir); err == nil {
		t.Fatal("损坏认证不能视为首次启动")
	}
}
