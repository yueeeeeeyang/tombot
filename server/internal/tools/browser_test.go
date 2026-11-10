package tools

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"io.yueyang/tom/internal/service"
)

func TestBrowserDownloadRequiresIndividualApprovalAndPublishesArtifact(t *testing.T) {
	t.Parallel()
	e, bot, run := fixture(t, "loose")
	bot.ToolPolicy.AllowedTools = []string{"browser"}
	tab, err := e.browser.Open(context.Background(), service.BrowserOpenInput{URL: "https://example.com"}, &run, "bot")
	if err != nil {
		t.Fatal(err)
	}
	result := execute(t, e, bot, run, "browser", map[string]any{"action": "download", "id": tab.ID, "url": "https://files.example/report"})
	var downloaded service.BrowserDownload
	if err = json.Unmarshal([]byte(result.Content), &downloaded); err != nil || result.IsError || downloaded.File.Path == "" {
		t.Fatal("下载工具没有产物", err, result.Content)
	}
	file, err := e.files.Download(context.Background(), bot.ID, downloaded.File.Path)
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(file.File)
	closeErr := file.File.Close()
	if readErr != nil || closeErr != nil || string(content) != "测试下载内容" {
		t.Fatal("工具产物内容不正确", readErr, closeErr)
	}
	invalid := execute(t, e, bot, run, "browser", map[string]any{"action": "download", "id": tab.ID, "url": "https://files.example/report", "ref": "e1"})
	if !invalid.IsError {
		t.Fatal("下载接受含糊的双目标")
	}
}

func TestBrowserToolNewOriginAndForcedInteractionApproval(t *testing.T) {
	t.Parallel()
	e, bot, run := fixture(t, "loose")
	bot.ToolPolicy.AllowedTools = []string{"browser"}
	result := execute(t, e, bot, run, "browser", map[string]any{"action": "open", "url": "https://example.com"})
	var tab service.BrowserTab
	if err := json.Unmarshal([]byte(result.Content), &tab); err != nil || result.IsError || tab.Controller != "bot" {
		t.Fatal("打开结果错误", err, result.Content)
	}
	for _, action := range []string{"tabs", "snapshot", "screenshot"} {
		input := map[string]any{"action": action}
		if action != "tabs" {
			input["id"] = tab.ID
		}
		result = execute(t, e, bot, run, "browser", input)
		if result.IsError {
			t.Fatal(result.Content)
		}
	}
	result = execute(t, e, bot, run, "browser", map[string]any{"action": "open", "id": tab.ID, "url": "https://example.com/next"})
	if result.IsError {
		t.Fatal("同源访问失败", result.Content)
	}
	result = execute(t, e, bot, run, "browser", map[string]any{"action": "type", "id": tab.ID, "ref": "e1", "text": "测试内容"})
	if result.IsError {
		t.Fatal(result.Content)
	}
}

func TestBrowserToolTakeoverInvalidatesPendingAction(t *testing.T) {
	t.Parallel()
	e, bot, run := fixture(t, "loose")
	bot.ToolPolicy.AllowedTools = []string{"browser"}
	tab, err := e.browser.Open(context.Background(), service.BrowserOpenInput{URL: "https://example.com"}, &run, "bot")
	if err != nil {
		t.Fatal(err)
	}
	before := tab.PageRevision
	tab, err = e.browser.SetController(context.Background(), tab.ID, "user", tab.Revision, "user")
	if err != nil {
		t.Fatal(err)
	}
	result := execute(t, e, bot, run, "browser", map[string]any{"action": "key", "id": tab.ID, "key": "Enter"})
	if !result.IsError || !strings.Contains(result.Content, "BROWSER_CONTROL_REQUIRED") {
		t.Fatal("用户控制时仍执行按键", result.Content)
	}
	tab, err = e.browser.Get(context.Background(), tab.ID)
	if err != nil || tab.PageRevision != before {
		t.Fatal("被拒绝的按键改变了页面", err)
	}
}

func TestBrowserToolPermissionRevokedWhileWaiting(t *testing.T) {
	for _, scenario := range []string{"新增拒绝规则", "撤回来源许可"} {
		t.Run(scenario, func(t *testing.T) {
			e, bot, run := fixture(t, "strict")
			bot.ToolPolicy.AllowedTools = []string{"browser"}
			tab, err := e.browser.Open(context.Background(), service.BrowserOpenInput{URL: "https://example.com"}, &run, "bot")
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "新增拒绝规则" {
				result := execute(t, e, bot, run, "browser", map[string]any{"action": "key", "id": tab.ID, "key": "Enter"})
				if result.IsError {
					t.Fatal("没有拒绝规则时按键失败", result.Content)
				}
				return
			}
			if _, err = e.db.Exec("DELETE FROM browser_origin_permissions WHERE tab_id=?", tab.ID); err != nil {
				t.Fatal(err)
			}
			allowed, err := e.browser.CanVisit(context.Background(), bot.ID, tab.ID, "https://example.com/next")
			if err != nil || allowed {
				t.Fatal("来源许可未被撤回", err)
			}
			result := execute(t, e, bot, run, "browser", map[string]any{"action": "open", "id": tab.ID, "url": "https://example.com/next"})
			if result.IsError {
				t.Fatal("显式导航没有立即执行", result.Content)
			}
			allowed, err = e.browser.CanVisit(context.Background(), bot.ID, tab.ID, "https://example.com/next")
			if err != nil || !allowed {
				t.Fatal("显式导航没有重新记录来源", err)
			}
		})
	}
}

func TestBrowserToolDenialInvalidArgumentsAndCrossOwner(t *testing.T) {
	t.Parallel()
	e, bot, run := fixture(t, "loose")
	bot.ToolPolicy.AllowedTools = []string{"browser"}
	for _, input := range []map[string]any{{"action": "evaluate"}, {"action": "open", "url": "file:///tmp/report"}, {"action": "key", "id": "missing", "key": "Arbitrary"}, {"action": "tabs", "id": "missing"}, {"action": "snapshot"}, {"action": "type", "id": "missing", "ref": "e1", "text": strings.Repeat("x", 16385)}} {
		result := execute(t, e, bot, run, "browser", input)
		if !result.IsError {
			t.Fatal("非法浏览器参数被接受")
		}
	}
	result := execute(t, e, bot, run, "browser", map[string]any{"action": "open", "url": "https://example.com"})
	if result.IsError {
		t.Fatal("新来源没有立即打开", result.Content)
	}
	tab, err := e.browser.Open(context.Background(), service.BrowserOpenInput{URL: "https://example.com"}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	result = execute(t, e, bot, run, "browser", map[string]any{"action": "snapshot", "id": tab.ID})
	if !result.IsError || !strings.Contains(result.Content, "BROWSER_TAB_FORBIDDEN") {
		t.Fatal("模型读取了未分配标签")
	}
}
