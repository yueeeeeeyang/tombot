package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/provision"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestRealBrowserScreenWebSocketUserFlow(t *testing.T) {
	archive := os.Getenv("TOM_TEST_BROWSER_ARCHIVE")
	if archive == "" {
		t.Skip("未提供浏览器归档，跳过真实浏览器 WebSocket 验收")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := provision.EnsureBrowser(ctx, root, archive); err != nil {
		t.Fatal(err)
	}
	var submitted atomic.Int64
	website := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		body := `<title>画面连接</title><form method="post" action="/done"><input name="text" style="position:absolute;left:20px;top:20px;width:240px;height:40px"><button>提交</button></form>`
		if r.URL.Path == "/done" && r.Method == "POST" {
			if err := r.ParseForm(); err != nil || r.Form.Get("text") != "实时中文输入" {
				t.Error("实时输入错误", err)
			}
			submitted.Add(1)
			body = `<title>完成</title><h1>提交完成</h1>`
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Error(err)
		}
	}))
	defer website.Close()
	s, code := fixtureWithBrowser(t, true, func(ctx context.Context, options browser.Options) (service.BrowserEngine, error) {
		options.Executable = provision.BrowserPath(root)
		options.ProfileDir = filepath.Join(root, "profile")
		return browser.New(ctx, options)
	})
	password := testkit.Random(t)
	if err := s.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := s.Auth.Login(ctx, password, "真实画面测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	tab, err := s.Browser.Open(ctx, service.BrowserOpenInput{URL: website.URL}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(s.Handler())
	defer host.Close()
	connection := dialBrowserScreen(t, host, session, tab.ID, "")
	defer func() {
		if err := connection.CloseNow(); err != nil {
			t.Log("真实画面连接已关闭")
		}
	}()
	frame := startBrowserScreen(t, connection, tab.ID)
	picture, err := base64.StdEncoding.DecodeString(frame["data"].(string))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(picture))
	if err != nil || decoded.Bounds().Dx() != browser.ViewportWidth || decoded.Bounds().Dy() != browser.ViewportHeight {
		t.Fatal("WebSocket 画面不是实际视口 JPEG", err)
	}
	for index, command := range []map[string]any{{"type": "click", "x": 80, "y": 40}, {"type": "input", "text": "实时中文输入"}, {"type": "key", "key": "Enter"}} {
		state := frame["tab"].(map[string]any)
		command["seq"], command["revision"], command["page_revision"] = index+1, state["revision"], state["page_revision"]
		sendBrowserScreen(t, connection, command)
		ack := readBrowserScreen(t, connection, "ack")
		version := ack["tab"].(map[string]any)["page_revision"].(float64)
		for {
			frame = readBrowserScreen(t, connection, "frame")
			current := frame["tab"].(map[string]any)
			if current["page_revision"].(float64) >= version && (index < 2 || current["title"] == "完成") {
				break
			}
			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}
		}
	}
	if submitted.Load() != 1 {
		t.Fatal("实时输入没有单次提交", submitted.Load())
	}
	if err = connection.CloseNow(); err != nil {
		t.Fatal(err)
	}
	current, err := s.Browser.Get(ctx, tab.ID)
	if err != nil || current.Status != "active" || current.Controller != "user" {
		t.Fatal("画面断开改变浏览器标签", err)
	}
}
