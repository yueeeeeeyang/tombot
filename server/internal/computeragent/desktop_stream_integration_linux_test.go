//go:build linux && agentintegration && !cgo

package computeragent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"image/png"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
)

// 使用真实 TigerVNC 和受 UID/资源组核验的 socket；不把此测试当作 cgroup 冻结验证。
func TestDesktopStreamRealTigerVNC(t *testing.T) {
	if os.Getenv("TOM_AGENT_ENTRY_TEST") == "" {
		t.Fatal("真实桌面流测试必须运行在专用容器")
	}
	fixture := newFixture(t)
	session := fixture.command.Session
	directory, err := newDesktopDirectory(computer.DesktopRuntimeRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := directory.Close(); err != nil {
			t.Error(err)
		}
	}()
	var auth bytes.Buffer
	writeField := func(value []byte) {
		if err := binary.Write(&auth, binary.BigEndian, uint16(len(value))); err != nil {
			t.Fatal(err)
		}
		auth.Write(value)
	}
	if err := binary.Write(&auth, binary.BigEndian, uint16(256)); err != nil {
		t.Fatal(err)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	writeField([]byte(host))
	writeField([]byte(strconv.Itoa(session.DisplayNumber)))
	writeField([]byte("MIT-MAGIC-COOKIE-1"))
	credential := make([]byte, 16)
	if _, err := rand.Read(credential); err != nil {
		t.Fatal(err)
	}
	writeField(credential)
	authPath := session.RuntimeDirectory() + "/Xauthority"
	if err := os.WriteFile(authPath, auth.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(authPath, session.UID, session.GID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	display := ":" + strconv.Itoa(session.DisplayNumber)
	command := exec.CommandContext(ctx, "/usr/bin/Xtigervnc", display, "-geometry", "1280x800", "-depth", "24", "-auth", authPath, "-nolisten", "tcp", "-rfbport", "-1", "-rfbunixpath", session.RuntimeDirectory()+"/desktop.sock", "-rfbunixmode", "0600", "-SecurityTypes", "None", "-desktop", "Bot", "-AlwaysShared", "-AcceptSetDesktopSize=0")
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(session.UID), Gid: uint32(session.GID)}}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		err := command.Wait()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Error(err)
		}
	}()
	for {
		probe, err := openDesktopConnection(ctx, session, "/")
		if err == nil {
			if err = probe.Close(); err != nil {
				t.Fatal(err)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("真实 VNC 未就绪")
		case <-time.After(20 * time.Millisecond):
		}
	}
	scope := &kernelScope{logger: fixture.guard.logger, directory: directory, desktopGroup: "/", desktop: &managedDesktop{status: computer.DesktopStatus{Session: session, State: "ready"}}}
	request := computer.DesktopStream{Session: session, ViewerID: session.SessionID}
	wrong := request
	wrong.Session.BotID = fixture.command.Session.RunID
	if _, err := scope.OpenDesktopStreamFile(ctx, wrong); err == nil {
		t.Fatal("跨 Bot 身份获准观看")
	}
	file, err := scope.OpenDesktopStreamFile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.FileConn(file)
	if closeErr := file.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	defer connection.Close()
	relay := scope.streams[request.ViewerID]
	defer relay.Close()
	frame, err := captureDesktopFrame(ctx, connection)
	if err != nil {
		t.Fatal("真实 RFB 全帧读取失败", err)
	}
	config, err := png.DecodeConfig(bytes.NewReader(frame))
	if err != nil || config.Width != 1280 || config.Height != 800 {
		t.Fatal("桌面尺寸错误", err)
	}
	run := func(arguments ...string) string {
		command := exec.CommandContext(ctx, "/usr/bin/xdotool", arguments...)
		command.Env = []string{"PATH=/usr/bin:/bin", "DISPLAY=" + display, "XAUTHORITY=" + authPath}
		command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(session.UID), Gid: uint32(session.GID)}}
		output, err := command.Output()
		if err != nil {
			t.Fatal("查询真实 X11 输入失败", err)
		}
		return string(output)
	}
	run("mousemove", "100", "120")
	packet := []byte{5, 0, 1, 44, 1, 144}
	if _, err := connection.Write(packet); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if position := run("getmouselocation", "--shell"); !strings.Contains(position, "X=100\n") || !strings.Contains(position, "Y=120\n") {
		t.Fatal("只读观看注入了鼠标")
	}
	if err := relay.SetControl(true); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write(packet); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		position := run("getmouselocation", "--shell")
		if strings.Contains(position, "X=300\n") && strings.Contains(position, "Y=400\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("接管输入未到达 X11")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := relay.SetControl(false); err != nil {
		t.Fatal(err)
	}
	packet[2], packet[3] = 0, 80
	if _, err := connection.Write(packet); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if !strings.Contains(run("getmouselocation", "--shell"), "X=300\n") {
		t.Fatal("交还后仍可注入输入")
	}
	if err := relay.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Read(make([]byte, 1)); err == nil {
		t.Fatal("关闭后观看流未撤销")
	}
}
