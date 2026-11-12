package provision

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/config"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/testkit"
)

// 此用例需要只有环回网卡的临时 Linux 容器，以免对开发机安装软件或接触外网。
func TestIsolatedBootstrapOfflineRecoveryAndMissingGlyphs(t *testing.T) {
	if os.Getenv("TOM_TEST_ISOLATED_BOOTSTRAP") != "1" {
		t.Skip("仅在断网的一次性 Linux 容器运行此验收")
	}
	archive := os.Getenv("TOM_TEST_BROWSER_ARCHIVE")
	if archive == "" {
		t.Fatal("需提供官方离线浏览器归档")
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, network := range interfaces {
		if network.Flags&net.FlagUp != 0 && network.Flags&net.FlagLoopback == 0 {
			t.Fatal("请关闭验收容器外网")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cfg := config.Config{DataDir: t.TempDir(), WorkspaceDir: t.TempDir()}
	db := testkit.Database(t)
	events := bus.New(db)
	newEnvironment := func() *Provisioner {
		t.Helper()
		p, err := New(ctx, db, events, Steps(cfg, db, CommandRunner{}), 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := newEnvironment()
	if !p.Claim() {
		t.Fatal("无法领取首次引导")
	}
	if err = p.RunClaimed(ctx, false); err == nil || p.Ready() || !strings.Contains(p.Snapshot().Error, "官方离线") || strings.Contains(p.Snapshot().Error, "下载浏览器") {
		t.Fatal("缺少离线包时未阻断引导，或仍提示去下载", p.Snapshot(), err)
	}
	cfg.BrowserArchive = archive
	p = newEnvironment()
	if err = p.ClaimMaintenance(ctx, "setup", false); err != nil {
		t.Fatal(err)
	}
	if err = p.RunClaimed(ctx, false); err != nil || !p.Ready() {
		t.Fatal("使用离线包后未恢复", p.Snapshot(), err)
	}
	var clock string
	if err = db.QueryRow("SELECT status FROM capabilities WHERE key='selfcheck_clock'").Scan(&clock); err != nil || clock != "degraded" {
		t.Fatal("离线时钟未明确告警", clock, err)
	}

	// 字体清单和真实 Chromium 都使用同一个空目录，不修改容器的系统字体包。
	directory := t.TempDir()
	fc := filepath.Join(directory, "fonts.conf")
	if err = os.WriteFile(fc, []byte(`<fontconfig><reset-dirs/><dir>`+directory+`/fonts</dir><cachedir>`+directory+`/cache</cachedir></fontconfig>`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(directory, "fonts"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FONTCONFIG_FILE", fc)
	if err = p.ClaimMaintenance(ctx, "setup", true); err != nil {
		t.Fatal(err)
	}
	if err = p.RunClaimed(ctx, true); err == nil || p.Ready() || !strings.Contains(p.Snapshot().Error, "中文字体") {
		t.Fatal("缺少中文字体仍允许进入工作台", p.Snapshot(), err)
	}
	if err = CheckBrowser(ctx, BrowserPath(cfg.DataDir)); err == nil || !strings.Contains(err.Error(), "中文节点未使用受支持的中文字形") {
		t.Fatal("真实 Chromium 未准确定位缺少中文字形", err)
	}
}
