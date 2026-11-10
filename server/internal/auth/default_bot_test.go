package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestDefaultBotSeedPreservesMissingAuthenticationProtection(t *testing.T) {
	db := testkit.Database(t)
	dir := t.TempDir()
	authentication, err := New(db, dir)
	if err != nil {
		t.Fatal("迁移种子不应阻止首次认证初始化", err)
	}
	code, err := authentication.IssueSetupCode()
	if err != nil {
		t.Fatal(err)
	}
	if err = authentication.Setup(testkit.Random(t), code); err != nil {
		t.Fatal(err)
	}
	bots := service.NewBots(db, bus.New(db), t.TempDir())
	if err = bots.EnsureDefault(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = New(db, dir); err != nil {
		t.Fatal("默认助手创建后无法恢复既有认证", err)
	}
	if err = os.Remove(filepath.Join(dir, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = New(db, dir); err == nil {
		t.Fatal("存在默认助手和认证提交记录时，认证文件缺失被当成首次安装")
	}
	if _, err = db.Exec(`DELETE FROM system_state WHERE key='auth.setup'`); err != nil {
		t.Fatal(err)
	}
	if _, err = New(db, dir); err == nil {
		t.Fatal("已有业务数据但认证文件和标记丢失时没有阻断")
	}
}
