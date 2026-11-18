//go:build linux && browserintegration

package sharedbrowser

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

func TestNativeChromeDownloadsAndHumanControl(t *testing.T) {
	options, _, sessions := testOptions()
	directory, err := os.MkdirTemp("/workspace", "native-human-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	if err = os.Chown(directory, computer.BrowserUID, computer.DesktopSharedGID); err != nil {
		t.Fatal(err)
	}
	filename := "native-download-" + ids.New() + ".bin"
	content := bytes.Repeat([]byte{0, 1, 255, 13, 10}, 12000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
			if _, err := w.Write(content); err != nil {
				t.Error(err)
			}
		case "/state":
			value := "guest"
			if cookie, err := r.Cookie("fixture_login"); err == nil {
				value = cookie.Value
			}
			if _, err := io.WriteString(w, value); err != nil {
				t.Error(err)
			}
		case "/login":
			http.SetCookie(w, &http.Cookie{Name: "fixture_login", Value: "shared", Path: "/", HttpOnly: true})
			w.WriteHeader(204)
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if _, err := io.WriteString(w, `<!doctype html><meta charset="utf-8"><a style="position:absolute;left:20px;top:20px" href="/download">下载文件</a><button style="position:absolute;left:20px;top:100px;width:200px;height:50px" onclick="fetch('/login').then(()=>document.body.dataset.logged='yes')">登录测试站点</button><input id="typed" style="position:absolute;left:20px;top:200px;height:40px">`); err != nil {
				t.Error(err)
			}
		}
	}))
	defer server.Close()
	service := launchTestBrowser(t, filepath.Join(directory, "profile"), options).service
	addresses := []Address{{Session: sessions[0], WindowID: ids.New()}, {Session: sessions[1], WindowID: ids.New()}}
	for _, address := range addresses {
		if _, err = service.Open(t.Context(), address); err != nil {
			t.Fatal(err)
		}
		if _, err = service.Execute(t.Context(), address, Command{Sequence: 1, Kind: "navigate", URL: server.URL}); err != nil {
			t.Fatal(err)
		}
	}
	click := desktopPoint(t, service, addresses[0], Command{Sequence: 2, Kind: "click", X: 60, Y: 30})
	if _, err = service.Execute(t.Context(), addresses[0], click); err != nil {
		t.Fatal(err)
	}
	downloaded := filepath.Join(computer.DownloadDirectory, filename)
	t.Cleanup(func() {
		if err := os.Remove(downloaded); err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		actual, err := os.ReadFile(downloaded)
		if err == nil && bytes.Equal(actual, content) {
			break
		}
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("浏览器未将完整二进制保存到 Bot 下载目录")
		}
		time.Sleep(30 * time.Millisecond)
	}
	reader := exec.CommandContext(t.Context(), "/usr/bin/setpriv", fmt.Sprintf("--reuid=%d", sessions[0].UID), fmt.Sprintf("--regid=%d", sessions[0].GID), "--clear-groups", "/bin/cat", downloaded)
	actual, err := reader.Output()
	if err != nil || !bytes.Equal(actual, content) {
		t.Fatal("Bot 无法读取浏览器下载文件", err)
	}
	// 真正的人机交接屏障：自动化停止，而用户通过所属 X11 桌面依然可以点击登录。
	login := desktopPoint(t, service, addresses[0], Command{Kind: "click", X: 60, Y: 120})
	if err = service.HumanControl(t.Context(), sessions[0], true); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	item := service.windows[addresses[0].WindowID]
	service.mu.Unlock()
	if err = service.options.Native.Execute(t.Context(), sessions[0], item.nativeID, Command{Sequence: 3, Kind: "type", Text: "禁止"}); err == nil {
		t.Fatal("接管时自动化仍可输入")
	}
	if _, err = service.options.Native.Screenshot(t.Context(), sessions[0], item.nativeID); err == nil {
		t.Fatal("接管时自动化仍可读取登录画面")
	}
	humanContext, cancelHuman := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelHuman()
	human := exec.CommandContext(humanContext, "/usr/bin/xdotool", "mousemove", fmt.Sprint(login.X), fmt.Sprint(login.Y), "click", "1")
	human.Env = []string{"PATH=/usr/bin:/bin", fmt.Sprintf("DISPLAY=:%d", sessions[0].DisplayNumber), "XAUTHORITY=" + filepath.Join(sessions[0].RuntimeDirectory(), "Xauthority")}
	if err = human.Run(); err != nil {
		t.Fatal("所属桌面的人工作用失败", err)
	}
	waitValue(t, service, addresses[0], `document.body.dataset.logged`, "yes")
	waitValue(t, service, addresses[1], `fetch('/state').then(r=>r.text())`, "shared")
	if _, err = service.Screenshot(context.Background(), addresses[1]); err != nil {
		t.Fatal("一个 Bot 的接管阻塞了另一 Bot", err)
	}
	if err = service.HumanControl(t.Context(), sessions[0], false); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Screenshot(t.Context(), addresses[0]); err != nil {
		t.Fatal("接管结束没有恢复自动化", err)
	}
	// Ctrl+T 产生无 opener 的标签，仍须属于已经授权的原生窗口。
	tabContext, cancelTab := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelTab()
	newTab := exec.CommandContext(tabContext, "/usr/bin/xdotool", "key", "ctrl+t")
	newTab.Env = human.Env
	if err = newTab.Run(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		owned, err := service.List(t.Context(), sessions[0])
		if err != nil {
			t.Fatal(err)
		}
		if len(owned) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("用户新标签未继承所属 Bot")
		}
		time.Sleep(30 * time.Millisecond)
	}
	other, err := service.List(t.Context(), sessions[1])
	if err != nil || len(other) != 1 {
		t.Fatal("新标签出现在另一 Bot", err)
	}
	// 新标签存在时，指向旧页面的操作仍须回到旧标签。
	if _, err = service.Execute(t.Context(), addresses[0], Command{Sequence: 5, Kind: "navigate", URL: server.URL + "/state"}); err != nil {
		t.Fatal(err)
	}
	waitValue(t, service, addresses[0], `document.body.textContent`, "shared")
}
