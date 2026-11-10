package httpapi

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/provision"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

// TestNativeBrowserRecovery 在临时目录中缺少 Chromium 时耗尽真实重试，再通过工作台恢复并打开健康页。
func TestNativeBrowserRecovery(t *testing.T) {
	if os.Getenv("TOM_TEST_NATIVE_BROWSER_RECOVERY") != "1" {
		t.Skip("未启用原生浏览器故障恢复验收")
	}
	archive := os.Getenv("TOM_TEST_BROWSER_ARCHIVE")
	if runtime.GOOS != "darwin" || archive == "" {
		t.Fatal("需要 macOS 原生环境与固定版本浏览器归档")
	}
	root := t.TempDir()
	s, code := fixtureWithBrowser(t, true, func(ctx context.Context, options browser.Options) (service.BrowserEngine, error) {
		options.Executable = provision.BrowserPath(root)
		options.ProfileDir = filepath.Join(root, "profile")
		return browser.New(ctx, options)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	password := testkit.Random(t)
	if err := s.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := s.Auth.Login(ctx, password, "原生浏览器修复验收", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	bot, err := s.Bots.Create(ctx, service.BotInput{Template: "assistant"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Browser.Start(ctx); err == nil {
		t.Fatal("缺少执行文件却已启动浏览器")
	}
	wait, cancelWait := context.WithTimeout(ctx, 40*time.Second)
	defer cancelWait()
	for {
		wake := s.Bus.Wake()
		status, err := s.Browser.Status(wait)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == "failed" {
			if status.Available || status.RecoveryFailures == 0 {
				t.Fatal("重试耗尽后的能力未关闭", status)
			}
			break
		}
		select {
		case <-wake:
		case <-wait.Done():
			t.Fatal("未观察到真实重试耗尽", wait.Err())
		}
	}
	if err = provision.EnsureBrowser(ctx, root, archive); err != nil {
		t.Fatal(err)
	}
	runNativeBotScenario(t, s, bot, session.DeviceToken, "browser_recovery_test.dart")
	status, err := s.Browser.Status(ctx)
	if err != nil || !status.Available || status.State != "running" || status.RecoveryFailures != 0 {
		t.Fatal("恢复后的 Chromium 状态错误", status, err)
	}
	tabs, err := s.Browser.List(ctx, "")
	if err != nil || len(tabs) != 1 || tabs[0].Status != "active" || tabs[0].Controller != "user" {
		t.Fatal("恢复后未打开一个可交互的用户标签", err)
	}
	u, err := url.Parse(tabs[0].URL)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Path != "/health" {
		t.Fatal("实际浏览器未打开本机健康页", err)
	}
}
