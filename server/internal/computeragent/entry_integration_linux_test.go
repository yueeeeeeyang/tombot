//go:build linux && agentintegration

package computeragent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
)

// TestContainerAgentEntrypoint 必须在专用受限容器运行，固定目录只允许空白 tmpfs。
func TestContainerAgentEntrypoint(t *testing.T) {
	program := os.Getenv("TOM_AGENT_ENTRY_TEST")
	if program == "" || os.Geteuid() != 0 {
		t.Fatal("入口验收必须指定纯 Go 代理二进制并使用专用 root 受限容器")
	}
	entries, err := os.ReadDir(ControlDirectory)
	if err != nil || len(entries) != 0 {
		t.Fatal("入口验收控制目录必须是空白专属 tmpfs", err)
	}
	fixture := newFixture(t)
	configuration := Configuration{Version: ConfigurationVersion, Identity: fixture.guard.identity, Options: RuntimeOptions{EgressConnectTimeoutMS: 1000, EgressIdleTimeoutMS: 1000, EgressLifetimeMS: 10000, EgressMaximumConnections: 8, EgressMaximumTransferBytes: 1 << 20, MaximumActive: 4, CallTimeoutMS: 1000, CleanupTimeoutMS: 1000, RetryDelayMS: 10, CleanupAttempts: 2, PollIntervalMS: 10, ShutdownTimeoutMS: 3000, MaximumConnections: 2}}
	previous := configuration
	previous.Version = 3
	previousEncoded, err := agentchannel.Encode(previous)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ControlDirectory, ConfigName), previousEncoded, 0600); err != nil {
		t.Fatal(err)
	}
	oldCtx, cancelOld := context.WithTimeout(t.Context(), 3*time.Second)
	oldOutput, oldErr := exec.CommandContext(oldCtx, program).CombinedOutput()
	oldTimeout := oldCtx.Err()
	cancelOld()
	if oldErr == nil || oldTimeout != nil || !bytes.Contains(oldOutput, []byte("电脑代理部署实例配置无效")) {
		t.Fatal("真实入口未拒绝缺少浏览器恢复约定的旧配置", oldErr, string(oldOutput))
	}
	if _, err := os.Lstat(filepath.Join(ControlDirectory, SocketName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("旧配置仍建立了控制监听", err)
	}
	encoded, err := agentchannel.Encode(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(ControlDirectory, ConfigName), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	start := func() (*exec.Cmd, <-chan error, *bytes.Buffer) {
		t.Helper()
		command := exec.Command(program)
		output := &bytes.Buffer{}
		command.Stdout, command.Stderr = output, output
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		done := make(chan struct{})
		go func() { result <- command.Wait(); close(done) }()
		t.Cleanup(func() {
			select {
			case <-done:
			default:
				if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					t.Error(err)
				}
				<-done
			}
		})
		return command, result, output
	}
	command, result, output := start()
	socket := filepath.Join(ControlDirectory, SocketName)
	until := time.Now().Add(3 * time.Second)
	for {
		if stat, err := os.Lstat(socket); err == nil && stat.Mode()&os.ModeSocket != 0 {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("代理入口提前退出：%v，%s", err, output.String())
		default:
		}
		if time.Now().After(until) {
			t.Fatal("代理入口未建立监听")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for name, mode := range map[string]os.FileMode{"/workspace": os.ModeSetgid | 0770, "/workspace/tools": os.ModeSetgid | 0770, "/workspace/tools/bin": os.ModeSetgid | 0770, "/workspace/.tmp": os.ModeSetgid | 0770, "/run/bot-desktops": 0711, "/tmp/.X11-unix": os.ModeSticky | 0777, "/tmp/.ICE-unix": os.ModeSticky | 0777} {
		stat, err := os.Lstat(name)
		if err != nil || !stat.IsDir() || stat.Mode()&(os.ModePerm|os.ModeSetgid|os.ModeSticky) != mode {
			t.Fatal("代理未在接单前建立正确目录", name, err)
		}
		identity := stat.Sys().(*syscall.Stat_t)
		expectedGroup := uint32(0)
		if strings.HasPrefix(name, "/workspace") {
			expectedGroup = 20000
		}
		if identity.Uid != 0 || identity.Gid != expectedGroup {
			t.Fatal("运行目录身份错误", name)
		}
	}
	address, err := computer.EgressAddress()
	if err != nil {
		t.Fatal(err)
	}
	var egress net.Conn
	for {
		egress, err = net.DialTimeout("tcp4", address, time.Second)
		if err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("代理未建立环回出网监听", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = egress.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// 没有宿主端点时必须明确失败，绝不能尝试容器直连或返回成功。
	if _, err = io.WriteString(egress, "GET http://example.test/ HTTP/1.1\r\nHost: example.test\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	egressResponse, err := http.ReadResponse(bufio.NewReader(egress), nil)
	if err != nil || egressResponse.StatusCode != http.StatusBadGateway {
		t.Fatal("缺少宿主出网端点未明确失败", err)
	}
	if err := errors.Join(egressResponse.Body.Close(), egress.Close()); err != nil {
		t.Fatal(err)
	}
	// 同一 PID 命名空间的 root 不是宿主控制者，真实入口必须拒绝。
	connection, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: socket, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	if err = connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var response [1]byte
	_, readErr := connection.Read(response[:])
	if err = connection.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(readErr, io.EOF) {
		t.Fatal("容器内 root 对端被信任")
	}
	duplicateCtx, stop := context.WithTimeout(context.Background(), time.Second)
	duplicate := exec.CommandContext(duplicateCtx, program)
	err = duplicate.Run()
	duplicateTimeout := duplicateCtx.Err()
	stop()
	if err == nil || duplicateTimeout != nil {
		t.Fatal("双代理获得同一持久目录")
	}
	if err = command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		if err != nil {
			t.Fatalf("代理正常停服失败：%v，%s", err, output.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("代理未在停止期限内退出")
	}
	if _, err = os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("正常退出后遗留监听", err)
	}
	released, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatal("正常退出后遗留出网监听", err)
	}
	if err := released.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(ControlDirectory, "agent.lock")); err != nil {
		t.Fatal("持久锁记录被删除", err)
	}
	// 真实入口不能在同代次探针消费结果未知时重新接单，不能靠清空记录掩盖故障。
	probeRecord := ProbeSnapshot{Version: 1, Grant: probeGrant(fixture), Phase: "consumed"}
	encodedProbe, err := agentchannel.Encode(probeRecord)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ControlDirectory, probeRecordName), encodedProbe, 0600); err != nil {
		t.Fatal(err)
	}
	unknownCtx, stopUnknown := context.WithTimeout(context.Background(), 3*time.Second)
	unknown := exec.CommandContext(unknownCtx, program)
	unknownOutput, unknownErr := unknown.CombinedOutput()
	unknownTimeout := unknownCtx.Err()
	stopUnknown()
	if unknownErr == nil || unknownTimeout != nil || !strings.Contains(string(unknownOutput), "旧探针未确认关闭") || !strings.Contains(string(unknownOutput), fixture.guard.identity.ComputerID) {
		t.Fatalf("未知探针重启未拒绝：%v，%s", unknownErr, unknownOutput)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("探针恢复失败后仍建立了监听", err)
	}
	probeRecord.Phase = "closed"
	encodedProbe, err = agentchannel.Encode(probeRecord)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ControlDirectory, probeRecordName), encodedProbe, 0600); err != nil {
		t.Fatal(err)
	}
	// 共享 Chrome 的未确认记录同样阻止真实入口接单，不因探针已关闭而忽略它。
	browserRecord := BrowserSnapshot{Version: 1, Grant: browserGrant(fixture), Phase: "active"}
	encodedBrowser, err := agentchannel.Encode(browserRecord)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ControlDirectory, browserRecordName), encodedBrowser, 0600); err != nil {
		t.Fatal(err)
	}
	browserCtx, stopBrowser := context.WithTimeout(context.Background(), 3*time.Second)
	browserRestart := exec.CommandContext(browserCtx, program)
	browserOutput, browserErr := browserRestart.CombinedOutput()
	browserTimeout := browserCtx.Err()
	stopBrowser()
	if browserErr == nil || browserTimeout != nil || !strings.Contains(string(browserOutput), "旧实例未确认关闭") || !strings.Contains(string(browserOutput), fixture.guard.identity.ComputerID) {
		t.Fatalf("未知浏览器重启未拒绝：%v，%s", browserErr, browserOutput)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("浏览器恢复失败后仍建立监听", err)
	}
	browserRecord.Phase = "closed"
	encodedBrowser, err = agentchannel.Encode(browserRecord)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ControlDirectory, browserRecordName), encodedBrowser, 0600); err != nil {
		t.Fatal(err)
	}
	// 目录漂移必须在监听前失败，不允许静默修复后继续接单。
	if err = os.Chmod("/tmp", 0700); err != nil {
		t.Fatal(err)
	}
	driftCtx, stopDrift := context.WithTimeout(context.Background(), 3*time.Second)
	drift := exec.CommandContext(driftCtx, program)
	driftOutput, driftErr := drift.CombinedOutput()
	driftTimeout := driftCtx.Err()
	stopDrift()
	if driftErr == nil || driftTimeout != nil || !strings.Contains(string(driftOutput), "临时根目录权限") || !strings.Contains(string(driftOutput), fixture.guard.identity.ComputerID) {
		t.Fatalf("目录异常没有带业务标识明确报错：%v，%s", driftErr, driftOutput)
	}
	if _, err = os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("准备失败仍建立了控制监听", err)
	}
	if err = os.Chmod("/tmp", os.ModeSticky|0777); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(ControlDirectory, ConfigName), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	invalidCtx, stop := context.WithTimeout(context.Background(), time.Second)
	invalid := exec.CommandContext(invalidCtx, program)
	err = invalid.Run()
	invalidTimeout := invalidCtx.Err()
	stop()
	if err == nil || invalidTimeout != nil {
		t.Fatal("无效配置仍启动代理")
	}
}
