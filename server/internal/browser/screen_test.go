package browser

import (
	"bytes"
	"context"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/provision"
)

func TestBrowserScreenPointBoundaries(t *testing.T) {
	for _, point := range [][2]float64{{0, 0}, {1279.9, 719.9}} {
		if err := ValidatePoint(point[0], point[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, point := range [][2]float64{{-1, 0}, {0, -1}, {1280, 0}, {0, 720}, {math.NaN(), 1}, {1, math.Inf(1)}} {
		if err := ValidatePoint(point[0], point[1]); errorCode(err) != "INVALID_BROWSER_INPUT" {
			t.Fatal("非法视口坐标被接受")
		}
	}
}

func TestRealBrowserScreenCaptureAndUserInput(t *testing.T) {
	archive := os.Getenv("TOM_TEST_BROWSER_ARCHIVE")
	if archive == "" {
		t.Skip("未提供浏览器归档，跳过真实画面输入验收")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := provision.EnsureBrowser(ctx, root, archive); err != nil {
		t.Fatal(err)
	}
	var submitted atomic.Int64
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		body := `<title>画面输入</title><form method="post" action="/done"><input name="text" style="position:absolute;left:20px;top:20px;width:240px;height:40px"><button>提交</button></form>`
		if r.URL.Path == "/done" && r.Method == "POST" {
			if err := r.ParseForm(); err != nil || r.Form.Get("text") != "中文画面输入" {
				t.Error("真实焦点输入内容错误", err)
			}
			submitted.Add(1)
			body = `<title>完成</title><h1>提交完成</h1>`
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Error(err)
		}
	}))
	defer host.Close()
	m, err := New(ctx, Options{Executable: provision.BrowserPath(root), ProfileDir: filepath.Join(root, "profile"), Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := m.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	tab, err := m.Open(ctx, newTab("user", host.URL))
	if err != nil {
		t.Fatal(err)
	}
	selected := Selection{Owner: "user", ID: tab.ID}
	frame, err := m.CaptureFrame(ctx, selected, 60)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(frame.JPEG))
	if err != nil || decoded.Bounds().Dx() != ViewportWidth || decoded.Bounds().Dy() != ViewportHeight {
		t.Fatal("画面不是固定视口 JPEG", err)
	}
	selected.Revision = frame.Tab.Revision
	if err = m.ClickPoint(ctx, selected, 80, 40); err != nil {
		t.Fatal(err)
	}
	if err = m.InputText(ctx, selected, "不得输入"); errorCode(err) != "BROWSER_TAB_CHANGED" {
		t.Fatal("旧画面版本允许输入", err)
	}
	selected.Revision = 0
	frame, err = m.CaptureFrame(ctx, selected, 60)
	if err != nil {
		t.Fatal(err)
	}
	selected.Revision = frame.Tab.Revision
	if err = m.InputText(ctx, selected, "中文画面输入"); err != nil {
		t.Fatal(err)
	}
	selected.Revision = 0
	frame, err = m.CaptureFrame(ctx, selected, 60)
	if err != nil {
		t.Fatal(err)
	}
	selected.Revision = frame.Tab.Revision
	if err = m.Key(ctx, selected, "Enter"); err != nil {
		t.Fatal(err)
	}
	waitBrowser(t, func() bool { return submitted.Load() == 1 })
	selected.Revision = 0
	waitBrowser(t, func() bool {
		frame, err = m.CaptureFrame(ctx, selected, 60)
		return err == nil && frame.Tab.Title == "完成"
	})
	if submitted.Load() != 1 {
		t.Fatal("画面读取重放了提交")
	}
}
