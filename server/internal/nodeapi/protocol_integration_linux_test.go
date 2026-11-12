//go:build linux && nodeintegration && !cgo

package nodeapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

func nodeDirectory(t *testing.T) string {
	t.Helper()
	if os.Getenv("TOM_NODE_ENTRY_TEST") == "" || os.Geteuid() != 0 {
		t.Fatal("节点控制验收必须运行在临时 Linux root 测试容器")
	}
	directory, err := os.MkdirTemp("/run", "node-control-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	return directory
}

func startTestServer(t *testing.T, node *testNode, session computer.ExecutionSession) (*Server, *Listener) {
	t.Helper()
	listener, err := Listen(filepath.Join(nodeDirectory(t), "control.sock"), 20001)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(testOperations(node), testOptions(session.NodeID), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener.UnixListener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("节点服务没有有界停止")
		}
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	})
	return server, listener
}

func runNodeClient(t *testing.T, socket, mode string, session computer.ExecutionSession, uid, gid uint32) {
	t.Helper()
	encoded, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestLinuxNodeClientHelper$", "-test.v")
	command.Env = []string{"PATH=/usr/bin:/bin", "TOM_NODE_CLIENT_MODE=" + mode, "TOM_NODE_CLIENT_SOCKET=" + socket, "TOM_NODE_CLIENT_SESSION=" + string(encoded)}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid}}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("非 root 节点客户端失败：%v %s", err, output)
	}
}

func TestLinuxNodeClientHelper(t *testing.T) {
	mode := os.Getenv("TOM_NODE_CLIENT_MODE")
	if mode == "" {
		t.Skip("此入口仅由父测试以专用身份启动")
	}
	var session computer.ExecutionSession
	if json.Unmarshal([]byte(os.Getenv("TOM_NODE_CLIENT_SESSION")), &session) != nil {
		t.Fatal("测试身份无效")
	}
	client, err := NewClient(ClientOptions{NodeID: session.NodeID, Socket: os.Getenv("TOM_NODE_CLIENT_SOCKET"), CallTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inputs := testInputs(session)
	if mode == "denied" {
		if _, err := client.Ensure(ctx, inputs["ensure"].(computer.Specification)); err == nil {
			t.Fatal("未授权调用成功")
		}
		return
	}
	if mode == "failure" {
		_, err := client.Ensure(ctx, inputs["ensure"].(computer.Specification))
		var failure *computer.Failure
		if !errors.As(err, &failure) || failure.Code != "resource_conflict" || !failure.IsPermanent {
			t.Fatal("确认的失败分类丢失", err)
		}
		return
	}
	if mode == "unknown" {
		_, err := client.Ensure(ctx, inputs["ensure"].(computer.Specification))
		if err == nil || strings.Contains(err.Error(), "raw-host-output") {
			t.Fatal("未知调用伪成功或泄露原始错误")
		}
		return
	}
	if mode == "invalid" {
		input := inputs["ensure"].(computer.Specification)
		input.NodeID = ids.New()
		if _, err := client.Ensure(ctx, input); err == nil {
			t.Fatal("错误节点身份未拒绝")
		}
		return
	}
	if mode != "all" {
		t.Fatal("未知客户端测试模式")
	}
	for _, action := range []string{"heartbeat", "inspect", "ensure", "remove", "open", "renew", "close", "start", "read", "write", "resize", "desktop_start", "desktop_status", "desktop_capture", "desktop_image_read", "desktop_input", "agent_start", "agent_write", "agent_read"} {
		input := inputs[action]
		switch action {
		case "heartbeat":
			_, err = client.Heartbeat(ctx, input.(computer.NodeQuery))
		case "inspect":
			_, err = client.Inspect(ctx, input.(computer.Specification))
		case "ensure":
			_, err = client.Ensure(ctx, input.(computer.Specification))
		case "remove":
			_, err = client.Remove(ctx, input.(computer.Specification))
		case "open":
			_, err = client.Open(ctx, input.(computer.SessionCommand))
		case "renew":
			_, err = client.Renew(ctx, input.(computer.SessionCommand))
		case "close":
			_, err = client.Close(ctx, input.(computer.SessionCommand))
		case "start":
			_, err = client.StartTerminal(ctx, input.(computer.TerminalStart))
		case "read":
			_, err = client.ReadTerminal(ctx, input.(computer.TerminalRead))
		case "write":
			_, err = client.WriteTerminal(ctx, input.(computer.TerminalWrite))
		case "resize":
			err = client.ResizeTerminal(ctx, input.(computer.TerminalResize))
		case "desktop_start":
			_, err = client.StartDesktop(ctx, input.(computer.ExecutionSession))
		case "desktop_status":
			_, err = client.InspectDesktop(ctx, input.(computer.ExecutionSession))
		case "desktop_capture":
			_, err = client.CaptureDesktop(ctx, input.(computer.DesktopCapture))
		case "desktop_image_read":
			_, err = client.ReadDesktopImage(ctx, input.(computer.DesktopImageRead))
		case "desktop_input":
			_, err = client.InputDesktop(ctx, input.(computer.DesktopInput))
		case "agent_start":
			err = client.StartAgent(ctx, input.(computer.ExecutionSession))
		case "agent_write":
			write := input.(computer.AgentWrite)
			write.Bytes = make([]byte, computer.AgentChunkBytes)
			for index := range write.Bytes {
				write.Bytes[index] = byte(index % 251)
			}
			_, err = client.WriteAgent(ctx, write)
		case "agent_read":
			output, callErr := client.ReadAgent(ctx, input.(computer.AgentRead))
			err = callErr
			if err == nil && string(output.Bytes) != string([]byte{0, 1, 255}) {
				err = errors.New("二进制输出改变")
			}
		}
		if err != nil {
			t.Fatal(action, err)
		}
	}
}

func TestLinuxNodeIdentityAndPublicOperations(t *testing.T) {
	session := testSession()
	node := &testNode{}
	_, listener := startTestServer(t, node, session)
	runNodeClient(t, listener.path, "all", session, 20001, 20001)
	if node.calls.Load() != 19 {
		t.Fatal("没有完成全部受限动作", node.calls.Load())
	}
	for _, credentials := range [][2]uint32{{20002, 20001}, {20001, 20002}, {0, 0}} {
		runNodeClient(t, listener.path, "denied", session, credentials[0], credentials[1])
	}
	runNodeClient(t, listener.path, "invalid", session, 20001, 20001)
	if node.calls.Load() != 19 {
		t.Fatal("错误身份或目标仍调用了节点")
	}
	if _, err := Listen(listener.path, 20001); err == nil {
		t.Fatal("重复节点监听未拒绝")
	}
}

func TestLinuxNodeKnownAndUnknownFailure(t *testing.T) {
	for _, test := range []struct {
		mode    string
		failure error
	}{{"failure", &computer.Failure{Code: "resource_conflict", IsPermanent: true}}, {"unknown", errors.New("raw-host-output")}} {
		session := testSession()
		node := &testNode{fail: test.failure}
		_, listener := startTestServer(t, node, session)
		runNodeClient(t, listener.path, test.mode, session, 20001, 20001)
		if node.calls.Load() != 1 {
			t.Fatal("失败动作被传输层重发", node.calls.Load())
		}
	}
}

func TestLinuxNodeSocketProtectionAndStaleRecovery(t *testing.T) {
	directory := nodeDirectory(t)
	path := filepath.Join(directory, "control.sock")
	if err := os.WriteFile(path, []byte("owned-file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path, 20001); err == nil {
		t.Fatal("监听删除了普通文件")
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "owned-file" {
		t.Fatal("普通文件被修改")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(directory, "elsewhere"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path, 20001); err == nil {
		t.Fatal("接受符号链接端点")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	orphan, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	orphan.SetUnlinkOnClose(false)
	if err := os.Chown(path, 0, 20001); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0660); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path, 20001); err == nil {
		t.Fatal("删除了仍有监听者的端点")
	}
	if err := orphan.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := Listen(path, 20001)
	if err != nil {
		t.Fatal("无法恢复确已停止的旧端点", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("节点停止后未清理自身 socket")
	}
	if _, err := os.Lstat(path + ".lock"); err != nil {
		t.Fatal("稳定锁文件被删除")
	}
	if err := os.Chmod(directory, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path, 20001); err == nil {
		t.Fatal("接受可替换的端点父目录")
	}
}

// directPair 允许传输边界测试选择同进程内核身份，不更改生产构造器的非 root 限制。
func directPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	path := filepath.Join(nodeDirectory(t), "pair.sock")
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	left, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	right, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return left, right
}

func TestLinuxNodeLeaseTransitAndMalformedRequests(t *testing.T) {
	for _, mode := range []string{"迟到租约", "过期调用", "外来时间空间", "缺少租约", "多余租约", "错误节点", "外来描述符", "未知动作", "正确期限"} {
		t.Run(mode, func(t *testing.T) {
			session := testSession()
			node := &testNode{}
			server, err := NewServer(testOperations(node), testOptions(session.NodeID), testLogger())
			if err != nil {
				t.Fatal(err)
			}
			server.options.ClientUID, server.options.ClientGID = uint32(os.Geteuid()), uint32(os.Getegid())
			left, right := directPair(t)
			channel, _, err := agentchannel.New(left, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := channel.Close(); err != nil {
					t.Error(err)
				}
			}()
			command := testInputs(session)["open"].(computer.SessionCommand)
			callDeadline, err := agentchannel.FromDeadline(time.Now().Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			leaseExpires := time.Now().Add(150 * time.Millisecond)
			leaseDeadline, err := agentchannel.FromDeadline(leaseExpires)
			if err != nil {
				t.Fatal(err)
			}
			input, err := agentchannel.Encode(command)
			if err != nil {
				t.Fatal(err)
			}
			content := request{NodeID: session.NodeID, Action: "open", Deadline: callDeadline, LeaseDeadline: &leaseDeadline, Input: input}
			switch mode {
			case "迟到租约":
				time.Sleep(180 * time.Millisecond)
			case "过期调用":
				content.Deadline.ExpiresNS = 1
			case "外来时间空间":
				content.Deadline.TimeNamespace++
			case "缺少租约":
				content.LeaseDeadline = nil
			case "多余租约":
				content.Action = "close"
			case "错误节点":
				content.NodeID = ids.New()
			case "未知动作":
				content.Action = "docker_exec"
			case "正确期限":
				time.Sleep(30 * time.Millisecond)
			}
			body, err := agentchannel.Encode(content)
			if err != nil {
				t.Fatal(err)
			}
			message := agentchannel.Message{Version: 1, ID: ids.New(), Method: "node_control", Body: body}
			var files []*os.File
			if mode == "外来描述符" {
				file, err := os.Open("/dev/null")
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := file.Close(); err != nil {
						t.Error(err)
					}
				}()
				files = []*os.File{file}
				message.Files = 1
			}
			done := make(chan error, 1)
			go func() { done <- server.serveConnection(context.Background(), right) }()
			if err := channel.Send(context.Background(), message, files); err != nil {
				t.Fatal(err)
			}
			_, received, receiveErr := channel.Receive(context.Background())
			if err := agentchannel.CloseFiles(received); err != nil {
				t.Fatal(err)
			}
			serveErr := <-done
			if mode == "正确期限" {
				if receiveErr != nil || serveErr != nil || node.calls.Load() != 1 || node.deadline.After(leaseExpires) || time.Until(node.deadline) > 130*time.Millisecond {
					t.Fatal("传输耗时延长了租约", node.deadline, serveErr, receiveErr)
				}
			} else if node.calls.Load() != 0 || serveErr == nil || receiveErr == nil {
				t.Fatal("无效请求进入节点操作", mode, node.calls.Load(), serveErr, receiveErr)
			}
		})
	}
}

func TestLinuxNodeClientRejectsUncertainResponses(t *testing.T) {
	for _, mode := range []string{"断开", "错误请求标识", "错误节点", "错误动作", "返回描述符", "错误业务归属", "双重结果", "过大退出码", "取消"} {
		t.Run(mode, func(t *testing.T) {
			session := testSession()
			client, err := NewClient(ClientOptions{NodeID: session.NodeID, Socket: "/run/unused.sock", CallTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var dials atomic.Int64
			done := make(chan error, 1)
			client.dial = func(context.Context) (*net.UnixConn, error) {
				dials.Add(1)
				left, right := directPair(t)
				go func() {
					channel, _, err := agentchannel.New(right, time.Second)
					if err != nil {
						done <- errors.Join(err, right.Close())
						return
					}
					var failure error
					defer func() { done <- errors.Join(failure, channel.Close()) }()
					message, received, err := channel.Receive(context.Background())
					if err != nil {
						failure = err
						return
					}
					if err := agentchannel.CloseFiles(received); err != nil {
						failure = err
						return
					}
					if mode == "断开" {
						return
					}
					if mode == "取消" {
						cancel()
						return
					}
					var content request
					if err := agentchannel.Decode(message.Body, &content); err != nil {
						failure = err
						return
					}
					var input computer.TerminalStart
					if err := agentchannel.Decode(content.Input, &input); err != nil {
						failure = err
						return
					}
					output := computer.TerminalOutput{Text: "ok", Cursor: 2, Exited: true, ExitCode: 0}
					if mode == "过大退出码" {
						output.ExitCode = 256
					}
					if mode == "错误业务归属" {
						output.Cursor = 1
					}
					encoded, err := agentchannel.Encode(output)
					if err != nil {
						failure = err
						return
					}
					result := response{NodeID: session.NodeID, Action: "start", Result: encoded}
					if mode == "错误节点" {
						result.NodeID = ids.New()
					}
					if mode == "错误动作" {
						result.Action = "remove"
					}
					if mode == "双重结果" {
						result.Failure = &computer.Failure{Code: "node_operation_unknown"}
					}
					body, err := agentchannel.Encode(result)
					if err != nil {
						failure = err
						return
					}
					reply := agentchannel.Message{Version: 1, ID: message.ID, Method: "result", Body: body}
					if mode == "错误请求标识" {
						reply.ID = ids.New()
					}
					var files []*os.File
					if mode == "返回描述符" {
						file, err := os.Open("/dev/null")
						if err != nil {
							failure = err
							return
						}
						defer func() { failure = errors.Join(failure, file.Close()) }()
						files = []*os.File{file}
						reply.Files = 1
					}
					failure = channel.Send(context.Background(), reply, files)
				}()
				return left, nil
			}
			_, err = client.StartTerminal(ctx, testInputs(session)["start"].(computer.TerminalStart))
			if err == nil || dials.Load() != 1 {
				t.Fatal("未知或伪造响应被接受或重发", mode, err, dials.Load())
			}
			if err := <-done; err != nil {
				t.Fatal("测试响应发送失败", err)
			}
		})
	}
}

type blockedComputer struct {
	*testNode
	entered chan struct{}
}

func (node blockedComputer) Ensure(ctx context.Context, _ computer.Specification) (computer.Observation, error) {
	node.calls.Add(1)
	close(node.entered)
	<-ctx.Done()
	return computer.Observation{}, ctx.Err()
}

func TestLinuxNodeCapacityAndCancellation(t *testing.T) {
	session := testSession()
	node := &testNode{}
	listener, err := Listen(filepath.Join(nodeDirectory(t), "control.sock"), 20001)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	operations := testOperations(node)
	operations.Computers = blockedComputer{node, entered}
	options := testOptions(session.NodeID)
	options.MaximumConnections = 1
	server, err := NewServer(operations, options, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	// 此测试检查连接调度，内核非 root 身份已由独立子进程组覆盖。
	server.options.ClientUID, server.options.ClientGID = uint32(os.Geteuid()), uint32(os.Getegid())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener.UnixListener) }()
	client, err := NewClient(ClientOptions{NodeID: session.NodeID, Socket: listener.path, CallTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	input := testInputs(session)["ensure"].(computer.Specification)
	go func() { _, err := client.Ensure(context.Background(), input); first <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("首个节点动作未到达")
	}
	if _, err := client.Ensure(context.Background(), input); err == nil {
		t.Error("超额连接执行了节点动作")
	}
	cancel()
	if err := <-first; err == nil {
		t.Error("取消后的节点动作伪成功")
	}
	if err := <-done; err != nil {
		t.Error(err)
	}
	if node.calls.Load() != 1 {
		t.Error("容量上限未阻止第二次操作")
	}
	if err := listener.Close(); err != nil {
		t.Error(err)
	}
}

type unfinishedComputer struct {
	*testNode
	entered chan struct{}
	release chan struct{}
}

func (node unfinishedComputer) Ensure(ctx context.Context, specification computer.Specification) (computer.Observation, error) {
	close(node.entered)
	<-node.release
	return computer.Observation{}, ctx.Err()
}

func TestLinuxNodeUnfinishedOperationRequiresExit(t *testing.T) {
	session := testSession()
	listener, err := Listen(filepath.Join(nodeDirectory(t), "control.sock"), 20001)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	}()
	node := unfinishedComputer{testNode: &testNode{}, entered: make(chan struct{}), release: make(chan struct{})}
	operations := testOperations(node.testNode)
	operations.Computers = node
	server, err := NewServer(operations, testOptions(session.NodeID), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	// 对端身份另由降权子进程覆盖，此处只验证在途操作未停止的退出契约。
	server.options.ClientUID, server.options.ClientGID = uint32(os.Geteuid()), uint32(os.Getegid())
	server.options.ShutdownTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener.UnixListener) }()
	client, err := NewClient(ClientOptions{NodeID: session.NodeID, Socket: listener.path, CallTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	response := make(chan error, 1)
	go func() {
		_, err := client.Ensure(t.Context(), testInputs(session)["ensure"].(computer.Specification))
		response <- err
	}()
	defer func() { close(node.release); <-response }()
	select {
	case <-node.entered:
	case <-time.After(time.Second):
		t.Fatal("动作未开始")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnfinishedOperations) {
			t.Fatal("未知在途动作未要求进程退出", err)
		}
	case <-time.After(time.Second):
		t.Fatal("节点未有界退出")
	}
}
