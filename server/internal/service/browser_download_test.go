package service

import (
	"context"
	"errors"
	"io"
	"testing"

	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/workspace"
)

func TestBrowserDownloadVersionsOwnershipAndStorageFailures(t *testing.T) {
	t.Parallel()
	s, engine, bot, run := browserServiceFixture(t)
	ctx := context.Background()
	tab, err := s.Open(ctx, BrowserOpenInput{URL: "https://example.com"}, &run, "bot")
	if err != nil {
		t.Fatal(err)
	}
	input := BrowserDownloadInput{BrowserVersion: browserVersion(tab), DownloadInput: browser.DownloadInput{URL: "https://files.example.com/report"}}
	saved := 0
	save := func(ctx context.Context, owner, name string, source io.Reader) (workspace.WriteResult, error) {
		content, err := io.ReadAll(source)
		if owner != bot.ID || name != "report.txt" || string(content) != "测试下载内容" {
			t.Error("下载保存归属或内容错误")
		}
		saved++
		return workspace.WriteResult{Path: "inbox/report.txt", Bytes: len(content)}, err
	}
	if _, err = s.Download(ctx, tab.ID, input, nil, "user", save); browserCode(err) != "BROWSER_CONTROL_REQUIRED" {
		t.Fatal("未接管用户可下载", err)
	}
	result, err := s.Download(ctx, tab.ID, input, &run, "bot", save)
	if err != nil || saved != 1 || result.File.Path == "" {
		t.Fatal("下载未发布", err)
	}
	if allowed, err := s.CanVisit(ctx, bot.ID, tab.ID, input.URL); err != nil || !allowed {
		t.Fatal("已批准来源未保存", err)
	}
	if _, err = s.Download(ctx, tab.ID, input, &run, "bot", save); browserCode(err) != "BROWSER_TAB_CHANGED" || saved != 1 {
		t.Fatal("旧版本重复下载", err)
	}
	input.BrowserVersion = browserVersion(result.Tab)
	failure := errors.New("存储写入失败")
	result, err = s.Download(ctx, tab.ID, input, &run, "bot", func(context.Context, string, string, io.Reader) (workspace.WriteResult, error) {
		return workspace.WriteResult{}, failure
	})
	if !errors.Is(err, failure) || result.Tab.Status != "active" {
		t.Fatal("存储失败没有保留标签及错误", err)
	}
	if len(engine.Operations()) != 3 {
		t.Fatal("拒绝或失败发生重放", engine.Operations())
	}
}

func TestBrowserDownloadAuditFailurePreventsNetwork(t *testing.T) {
	t.Parallel()
	s, engine, _, run := browserServiceFixture(t)
	ctx := context.Background()
	tab, err := s.Open(ctx, BrowserOpenInput{URL: "https://example.com"}, &run, "bot")
	if err != nil {
		t.Fatal(err)
	}
	saved := 0
	_, err = s.Download(ctx, tab.ID, BrowserDownloadInput{BrowserVersion: browserVersion(tab), DownloadInput: browser.DownloadInput{URL: "https://files.example/report"}}, &run, "bot", func(context.Context, string, string, io.Reader) (workspace.WriteResult, error) {
		saved++
		return workspace.WriteResult{Path: "inbox/report"}, nil
	})
	if err != nil || saved != 1 || len(engine.Operations()) != 2 {
		t.Fatal("下载未立即执行", err, saved, len(engine.Operations()))
	}
	if allowed, err := s.CanVisit(ctx, run.BotID, tab.ID, "https://files.example/report"); err != nil || !allowed {
		t.Fatal("下载未记录来源", err)
	}
}
