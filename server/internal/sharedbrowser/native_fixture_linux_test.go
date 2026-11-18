//go:build linux && browserintegration

package sharedbrowser

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
)

// startNativeTestDesktops 为真实浏览器测试建立每个 Bot 独立的显示服务；控制文件不进入共享目录。
func startNativeTestDesktops(t *testing.T, sessions []computer.ExecutionSession) {
	t.Helper()
	for _, session := range sessions {
		if _, err := os.Stat(filepath.Join(session.RuntimeDirectory(), "Xauthority")); err == nil {
			// 同一测试关闭并重启 Chrome 时仍复用原桌面和认证。
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	command := exec.Command("/usr/bin/python3", "-I", "-B", "testdata/native_desktops.py")
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = io.Discard
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		if err := input.Close(); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(8 * time.Second):
			if err := command.Process.Kill(); err != nil {
				t.Error(err)
			}
			<-done
			t.Error("测试桌面回收超时")
		}
		for _, session := range sessions {
			if err := os.RemoveAll(session.RuntimeDirectory()); err != nil {
				t.Error(err)
			}
		}
	})
	if err = json.NewEncoder(input).Encode(sessions); err != nil {
		t.Fatal(err)
	}
	ready := make(chan string, 1)
	go func() {
		line, err := bufio.NewReader(output).ReadString('\n')
		if err != nil {
			ready <- "failed"
		} else {
			ready <- line
		}
	}()
	select {
	case line := <-ready:
		if line != "ready\n" {
			t.Fatal("真实测试桌面未启动")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("真实测试桌面启动超时")
	}
}

// desktopPoint 把测试页面上的既定点击目标转为用户实际桌面的坐标，不代替真实鼠标输入。
func desktopPoint(t *testing.T, service *Service, address Address, command Command) Command {
	t.Helper()
	if command.Kind != "click" && command.Kind != "double_click" && command.Kind != "move" && command.Kind != "scroll" {
		return command
	}
	value := evaluate(t, service, address, `JSON.stringify({x:window.screenX,y:window.screenY,top:window.outerHeight-window.innerHeight})`)
	var bounds struct {
		X   int `json:"x"`
		Y   int `json:"y"`
		Top int `json:"top"`
	}
	if json.Unmarshal([]byte(value), &bounds) != nil {
		t.Fatal("无法读取测试浏览器边界")
	}
	// Xpra 保留服务端窗口坐标；客户端固定停靠位置由窗口显示回执读取。
	native, ok := service.options.Native.(nativeBrowser)
	if !ok {
		t.Fatal("测试缺少真实原生桌面")
	}
	service.mu.Lock()
	item := service.windows[address.WindowID]
	service.mu.Unlock()
	receipt, err := native.call(context.Background(), nativeBrowserRequest{Action: "position", Owner: address.Session.SessionID, Window: item.nativeID})
	if err != nil || receipt.State != "ready" {
		t.Fatal("读取测试窗口位置失败", err)
	}
	command.X += receipt.X
	command.Y += receipt.Y + bounds.Top
	return command
}
