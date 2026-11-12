//go:build linux && browserintegration

package sharedbrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

// TestLocalBrowserClientProcess 在真实降权子进程调用正式本机客户端；无需服务端浏览器 RPC。
func TestLocalBrowserClientProcess(t *testing.T) {
	encoded := os.Getenv("TOM_LOCAL_CLIENT_SESSION")
	if encoded == "" {
		return
	}
	var session computer.ExecutionSession
	if json.Unmarshal([]byte(encoded), &session) != nil {
		t.Fatal("测试会话无效")
	}
	client, err := NewClient(session)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.http.Transport
	client.http.Transport = clientTransport(func(request *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(request)
		if err != nil {
			t.Log("本机传输失败", err)
		} else if response.StatusCode != 200 {
			t.Log("本机接口状态", response.StatusCode, request.URL.Path)
		}
		return response, err
	})
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	id := os.Getenv("TOM_LOCAL_CLIENT_WINDOW")
	if _, err = client.Open(ctx, id); err != nil {
		t.Fatal("本机创建失败", err)
	}
	window, err := client.Execute(ctx, id, Command{Sequence: 1, Kind: "navigate", URL: os.Getenv("TOM_LOCAL_CLIENT_URL")})
	if err != nil || window.CommandState != "completed" {
		t.Fatal("本机导航失败", window.CommandState, err)
	}
	pixels, err := client.Screenshot(ctx, id)
	if err != nil || !bytes.HasPrefix(pixels, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatal("本机截图失败", err)
	}
	state, err := client.State(ctx)
	if err != nil || len(state.Windows) != 1 || state.Windows[0].ID != id {
		t.Fatal("本机页面归属错误", err)
	}
	if foreign := os.Getenv("TOM_LOCAL_CLIENT_FOREIGN"); foreign != "" {
		if _, err := client.Screenshot(ctx, foreign); !errors.Is(err, ErrDenied) {
			t.Fatal("本机访问其他Bot页面", err)
		}
	}
	if _, err := fmt.Fprintln(os.Stdout, state.Windows[0].Title); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestSharedChromeLocalAgentClient(t *testing.T) {
	options, access, sessions := testOptions()
	directory, err := os.MkdirTemp("/workspace", "local-browser-client-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(directory, "profile")
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(profile, computer.BrowserUID, computer.DesktopSharedGID); err != nil {
		t.Fatal(err)
	}
	chrome := launchTestBrowser(t, profile, options)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value := "first"
		if _, err := r.Cookie("shared_test_state"); err == nil {
			value = "shared"
		}
		http.SetCookie(w, &http.Cookie{Name: "shared_test_state", Value: "visible", Path: "/"})
		if _, err := fmt.Fprintf(w, "<html><head><title>%s</title></head><body>本机浏览器<input id=files type=file multiple onchange='window.fileChanges=(window.fileChanges||0)+1'><input id=text></body></html>", value); err != nil {
			t.Error(err)
		}
	}))
	defer site.Close()
	if err := os.MkdirAll(computer.DesktopRuntimeRoot, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(computer.DesktopRuntimeRoot, 0711); err != nil {
		t.Fatal(err)
	}
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// go test 的构建父目录仅允许 root 遍历；将测试程序复制到独立可执行目录，不修改生产权限。
	clientDirectory, err := os.MkdirTemp("/tmp", "local-browser-process-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(clientDirectory); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(clientDirectory, 0755); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(program)
	if err != nil {
		t.Fatal(err)
	}
	program = filepath.Join(clientDirectory, "client.test")
	destination, err := os.OpenFile(program, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		t.Fatal(errors.Join(err, source.Close()))
	}
	_, copyError := io.Copy(destination, source)
	if err := errors.Join(copyError, source.Close(), destination.Close()); err != nil {
		t.Fatal(err)
	}
	var firstWindow string
	firstPath, secondPath := filepath.Join(directory, "共享甲.txt"), filepath.Join(directory, "共享乙.txt")
	for index, session := range sessions {
		directory := session.RuntimeDirectory()
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(directory, session.UID, session.GID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(directory); err != nil {
				t.Error(err)
			}
		})
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(directory, "browser.sock"), Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(listener.Addr().String(), 0, session.GID); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(listener.Addr().String(), 0660); err != nil {
			t.Fatal(err)
		}
		// 此夹具验证真实 UID、Unix 对端和 Chrome；生产 cgroup 正向证据仍由宿主验收提供。
		gateway, err := NewGateway(observedScreenshotBrowser{Browser: chrome.service, t: t}, session, func(ctx context.Context, current computer.ExecutionSession, _ Peer) error {
			return access.check(ctx, current)
		}, GatewayOptions{Width: options.Width, Height: options.Height, MaximumConnections: 4, CallTimeout: 6 * time.Second, ShutdownTimeout: time.Second}, options.Logger)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- gateway.Serve(ctx, listener) }()
		t.Cleanup(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(2 * time.Second):
				t.Error("本机入口未停止")
			}
		})
		identity, err := json.Marshal(session)
		if err != nil {
			t.Fatal(err)
		}
		window := ids.New()
		bounded, stop := context.WithTimeout(t.Context(), 15*time.Second)
		command := exec.CommandContext(bounded, program, "-test.run=^TestLocalBrowserClientProcess$")
		command.Env = []string{"PATH=/usr/bin:/bin", "TOM_LOCAL_CLIENT_SESSION=" + string(identity), "TOM_LOCAL_CLIENT_WINDOW=" + window, "TOM_LOCAL_CLIENT_URL=" + site.URL, "TOM_LOCAL_CLIENT_FOREIGN=" + firstWindow}
		command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(session.UID), Gid: uint32(session.GID)}}
		output, err := command.CombinedOutput()
		stop()
		if err != nil {
			t.Fatal("本机客户端进程失败", err, string(output))
		}
		if index == 1 && strings.TrimSpace(string(output)) != "shared" {
			t.Fatal("两个Bot未共享浏览器状态", string(output))
		}
		runNativeBrowserAgent(t, session, site.URL)
		for _, path := range []string{firstPath, secondPath} {
			if err := os.WriteFile(path, []byte("上传原文"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		runNativeBrowserAgentWithUpload(t, session, site.URL, []string{firstPath, secondPath}, func(id string) error {
			if err := os.WriteFile(firstPath, []byte("原文件随后改变"), 0644); err != nil {
				return err
			}
			address := Address{Session: session, WindowID: id}
			actual := evaluate(t, chrome.service, address, `Promise.all(Array.from(document.querySelector('#files').files).map(async file=>file.name+':'+await file.text())).then(files=>files.join('|')+'|'+window.fileChanges)`)
			if actual != "共享甲.txt:上传原文|共享乙.txt:上传原文|1" {
				return errors.New("网页未收到固定文件内容或选择事件重复")
			}
			return nil
		})
		chrome.service.options.Uploads.mu.Lock()
		remaining := len(chrome.service.options.Uploads.batches)
		chrome.service.options.Uploads.mu.Unlock()
		if remaining != 0 {
			t.Fatal("关闭 Agent 页面后遗留上传副本", remaining)
		}
		firstWindow = window
	}
}

// observedScreenshotBrowser 在回归失败时保留原始捕获原因，不补发截图掩盖首个失败。
type observedScreenshotBrowser struct {
	Browser
	t *testing.T
}

func (browser observedScreenshotBrowser) Screenshot(ctx context.Context, address Address) ([]byte, error) {
	pixels, err := browser.Browser.Screenshot(ctx, address)
	if err != nil {
		browser.t.Log("真实捕获失败", err)
	}
	return pixels, err
}

func (browser observedScreenshotBrowser) Upload(ctx context.Context, address Address, body io.Reader) (Window, error) {
	uploader, ok := browser.Browser.(UploadBrowser)
	if !ok {
		return Window{}, ErrUnavailable
	}
	return uploader.Upload(ctx, address, body)
}
