package nodeapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

type testNode struct {
	calls    atomic.Int64
	deadline time.Time
	fail     error
}

func (node *testNode) enter() error { node.calls.Add(1); return node.fail }
func (node *testNode) Heartbeat(_ context.Context, input computer.NodeQuery) (computer.NodeHeartbeat, error) {
	return computer.NodeHeartbeat{NodeID: input.NodeID, InstanceID: input.NodeID, Generation: 1, Sequence: 1}, node.enter()
}
func (node *testNode) Inspect(_ context.Context, input computer.Specification) (computer.Observation, error) {
	return testObservation(input, "absent"), node.enter()
}
func (node *testNode) Ensure(_ context.Context, input computer.Specification) (computer.Observation, error) {
	return testObservation(input, "ready"), node.enter()
}
func (node *testNode) Remove(_ context.Context, input computer.Specification) (computer.Observation, error) {
	return testObservation(input, "absent"), node.enter()
}
func (node *testNode) Open(ctx context.Context, input computer.SessionCommand) (computer.SessionObservation, error) {
	node.deadline = computer.LeaseDeadline(ctx, input.Lifetime)
	return computer.SessionObservation{Session: input.Session, Revision: input.Revision, State: "active"}, node.enter()
}
func (node *testNode) Renew(ctx context.Context, input computer.SessionCommand) (computer.SessionObservation, error) {
	return node.Open(ctx, input)
}
func (node *testNode) Close(_ context.Context, input computer.SessionCommand) (computer.SessionObservation, error) {
	return computer.SessionObservation{Session: input.Session, Revision: input.Revision, State: "drained"}, node.enter()
}
func (node *testNode) StartTerminal(context.Context, computer.TerminalStart) (computer.TerminalOutput, error) {
	return computer.TerminalOutput{Text: "容器", Cursor: 6, Exited: true, ExitCode: 0}, node.enter()
}
func (node *testNode) ReadTerminal(_ context.Context, input computer.TerminalRead) (computer.TerminalOutput, error) {
	return computer.TerminalOutput{Text: "后续", Cursor: input.Cursor + 6, Exited: true, ExitCode: 0}, node.enter()
}
func (node *testNode) WriteTerminal(_ context.Context, input computer.TerminalWrite) (computer.TerminalReceipt, error) {
	return computer.TerminalReceipt{Sequence: input.Sequence, WrittenBytes: len(input.Text)}, node.enter()
}
func (node *testNode) ResizeTerminal(context.Context, computer.TerminalResize) error {
	return node.enter()
}
func (node *testNode) StartDesktop(_ context.Context, input computer.ExecutionSession) (computer.DesktopStatus, error) {
	return computer.DesktopStatus{Session: input, State: "ready", Width: computer.DesktopWidth, Height: computer.DesktopHeight}, node.enter()
}
func (node *testNode) InspectDesktop(ctx context.Context, input computer.ExecutionSession) (computer.DesktopStatus, error) {
	return node.StartDesktop(ctx, input)
}
func testDesktopImage(input computer.DesktopCapture) computer.DesktopImage {
	return computer.DesktopImage{DesktopCapture: input, Width: computer.DesktopWidth, Height: computer.DesktopHeight, MediaType: "image/png", Size: computer.MaximumDesktopChunkBytes, SHA256: strings.Repeat("a", 64)}
}
func (node *testNode) CaptureDesktop(_ context.Context, input computer.DesktopCapture) (computer.DesktopImage, error) {
	return testDesktopImage(input), node.enter()
}
func (node *testNode) ReadDesktopImage(_ context.Context, input computer.DesktopImageRead) (computer.DesktopImageChunk, error) {
	return computer.DesktopImageChunk{Image: testDesktopImage(input.DesktopCapture), Offset: input.Offset, Bytes: make([]byte, min(input.MaximumBytes, computer.MaximumDesktopChunkBytes-input.Offset))}, node.enter()
}
func (node *testNode) InputDesktop(_ context.Context, input computer.DesktopInput) (computer.DesktopInputReceipt, error) {
	return computer.DesktopInputReceipt{Session: input.Session, Sequence: input.Sequence, State: "sent"}, node.enter()
}
func (node *testNode) StartAgent(context.Context, computer.ExecutionSession) error {
	return node.enter()
}
func (node *testNode) WriteAgent(_ context.Context, input computer.AgentWrite) (int64, error) {
	return input.Offset + int64(len(input.Bytes)), node.enter()
}
func (node *testNode) ReadAgent(_ context.Context, input computer.AgentRead) (computer.AgentOutput, error) {
	return computer.AgentOutput{Offset: input.Offset + 3, Bytes: []byte{0, 1, 255}, ExitCode: -1}, node.enter()
}

func testObservation(input computer.Specification, state string) computer.Observation {
	value := computer.Observation{OperationID: input.OperationID, UserID: input.UserID, ComputerID: input.ComputerID, Generation: input.Generation, FencingToken: input.FencingToken, VolumeReference: input.VolumeReference, State: state}
	if state == "ready" {
		value.ContainerID = strings.Repeat("a", 64)
		value.Capabilities = computer.Capabilities{Terminal: true, Browser: true, Files: true, ProcessReaping: true, DiskQuota: true, NetworkIsolation: true}
	}
	return value
}
func testOperations(node *testNode) Operations {
	return Operations{Node: node, Computers: node, Sessions: node, Terminals: node, Desktops: node, Agents: node}
}
func testSession() computer.ExecutionSession {
	return computer.ExecutionSession{DesktopIdentity: computer.DesktopIdentity{UserID: ids.New(), ComputerID: ids.New(), BotID: ids.New(), DesktopID: ids.New(), UID: computer.DesktopUIDBase + 1, GID: computer.DesktopSharedGID, DisplayNumber: 1}, NodeID: ids.New(), ContainerID: strings.Repeat("a", 64), Generation: 1, SessionID: ids.New(), RunID: ids.New(), FencingToken: 1}
}
func testInputs(session computer.ExecutionSession) map[string]any {
	specification := computer.Specification{OperationID: ids.New(), UserID: session.UserID, ComputerID: session.ComputerID, NodeID: session.NodeID, ImageReference: "example.test/computer@sha256:" + strings.Repeat("a", 64), VolumeReference: "tombot-user-" + session.UserID, Generation: 1, FencingToken: 1, Attempt: 1, LeaseExpiresAt: time.Now().Add(time.Minute), Resources: computer.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 1 << 30, ProcessLimit: 128}}
	command := computer.SessionCommand{Session: session, Revision: 1, Lifetime: 3 * time.Second}
	address := computer.TerminalAddress{Session: session, OperationID: ids.New()}
	capture := computer.DesktopCapture{Session: session, Sequence: 1}
	return map[string]any{"heartbeat": computer.NodeQuery{NodeID: session.NodeID}, "inspect": specification, "ensure": specification, "remove": specification, "open": command, "renew": command, "close": command,
		"start": computer.TerminalStart{TerminalAddress: address, Command: computer.ProcessCommand{WorkingDirectory: "/workspace", Program: "/bin/bash", Arguments: []string{"-c", "pwd"}}, Columns: 100, Rows: 30},
		"read":  computer.TerminalRead{TerminalAddress: address, Cursor: 3, MaximumBytes: 64}, "write": computer.TerminalWrite{TerminalAddress: address, Sequence: 1, Text: "你好\n"}, "resize": computer.TerminalResize{TerminalAddress: address, Columns: 120, Rows: 40},
		"desktop_capture": capture, "desktop_image_read": computer.DesktopImageRead{DesktopCapture: capture, Offset: 0, MaximumBytes: computer.MaximumDesktopChunkBytes},
		"desktop_input": computer.DesktopInput{Session: session, Sequence: 1, Events: []computer.DesktopEvent{{Kind: "pointer", X: 20, Y: 30}}},
		"desktop_start": session, "desktop_status": session, "agent_start": session, "agent_write": computer.AgentWrite{Session: session, Offset: 3, Bytes: []byte{1, 0, 255}}, "agent_read": computer.AgentRead{Session: session, Offset: 3}}
}
func testOptions(nodeID string) ServerOptions {
	return ServerOptions{NodeID: nodeID, CallTimeout: time.Second, ShutdownTimeout: 2 * time.Second, MaximumConnections: 8}
}
func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestTypedNodeProtocol(t *testing.T) {
	session := testSession()
	node := &testNode{}
	operations := testOperations(node)
	for action, input := range testInputs(session) {
		encoded, err := agentchannel.Encode(input)
		if err != nil {
			t.Fatal(err)
		}
		decoded, lifetime, err := decodeInput(session.NodeID, action, encoded)
		if err != nil {
			t.Fatal(action, err)
		}
		if (action == "open" || action == "renew") != (lifetime > 0) {
			t.Fatal("租约期限分类错误", action)
		}
		output, err := operations.invoke(context.Background(), action, decoded)
		if err != nil || validateResult(action, decoded, output) != nil {
			t.Fatal("合法操作结果被拒绝", action, err)
		}
		if _, _, err := decodeInput(ids.New(), action, encoded); err == nil {
			t.Fatal("接受其他节点资源", action)
		}
		if _, _, err := decodeInput(session.NodeID, action, append(encoded, []byte(" {}")...)); err == nil {
			t.Fatal("接受尾随对象", action)
		}
		if _, _, err := decodeInput(session.NodeID, action, []byte(`{}`)); err == nil {
			t.Fatal("接受无身份内容", action)
		}
	}
	if node.calls.Load() != 19 {
		t.Fatal("协议未覆盖全部动作", node.calls.Load())
	}
	if _, _, err := decodeInput(session.NodeID, "docker_exec", []byte(`{}`)); err == nil {
		t.Fatal("接受通用宿主动作")
	}
}

func TestNodeResultBinding(t *testing.T) {
	session := testSession()
	inputs := testInputs(session)
	wrong := session
	wrong.BotID = ids.New()
	for _, test := range []struct {
		action string
		result any
	}{
		{"heartbeat", computer.NodeHeartbeat{NodeID: ids.New(), InstanceID: ids.New(), Generation: 1, Sequence: 1}},
		{"heartbeat", computer.NodeHeartbeat{NodeID: session.NodeID, InstanceID: ids.New(), Generation: 0, Sequence: 1}},
		{"heartbeat", computer.NodeHeartbeat{NodeID: session.NodeID, InstanceID: ids.New(), Generation: 1, Sequence: 0}},
		{"inspect", computer.Observation{State: "absent"}},
		{"ensure", func() computer.Observation {
			value := testObservation(inputs["ensure"].(computer.Specification), "ready")
			value.Capabilities.Browser = false
			return value
		}()},
		{"open", computer.SessionObservation{Session: wrong, Revision: 1, State: "active"}},
		{"close", computer.SessionObservation{Session: session, Revision: 1, State: "active"}},
		{"read", computer.TerminalOutput{Cursor: 2, ExitCode: -1}},
		{"write", computer.TerminalReceipt{Sequence: 1, WrittenBytes: 1}},
		{"resize", confirmation{}},
		{"agent_start", confirmation{}},
		{"desktop_start", computer.DesktopStatus{Session: wrong, State: "ready", Width: 1280, Height: 800}},
		{"desktop_capture", testDesktopImage(computer.DesktopCapture{Session: wrong, Sequence: 1})},
		{"desktop_image_read", computer.DesktopImageChunk{Image: testDesktopImage(inputs["desktop_capture"].(computer.DesktopCapture))}},
		{"desktop_input", computer.DesktopInputReceipt{Session: session, Sequence: 2, State: "sent"}},
		{"agent_write", agentReceipt{Offset: 4}},
		{"agent_read", computer.AgentOutput{Offset: 3, Exited: true, ExitCode: -1}},
	} {
		if validateResult(test.action, inputs[test.action], test.result) == nil {
			t.Fatal("接受错误结果", test.action)
		}
	}
}

func TestNodeConfigurationAndFailures(t *testing.T) {
	session := testSession()
	options := testOptions(session.NodeID)
	operations := testOperations(&testNode{})
	if _, err := NewServer(operations, options, testLogger()); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ServerOptions){func(o *ServerOptions) { o.NodeID = "bad" }, func(o *ServerOptions) { o.CallTimeout = 0 }, func(o *ServerOptions) { o.CallTimeout = 2 * time.Minute }, func(o *ServerOptions) { o.ShutdownTimeout = 0 }, func(o *ServerOptions) { o.MaximumConnections = 0 }, func(o *ServerOptions) { o.MaximumConnections = 1025 }} {
		invalid := options
		change(&invalid)
		if _, err := NewServer(operations, invalid, testLogger()); err == nil {
			t.Fatal("无效节点配置被接受")
		}
	}
	if _, err := NewServer(Operations{}, options, testLogger()); err == nil {
		t.Fatal("接受缺失能力")
	}
	if _, err := NewServer(operations, options, nil); err == nil {
		t.Fatal("接受缺失日志")
	}
	for _, endpoint := range []string{"", "http://node.example.test", "https://node.example.test/path", "https://user:pass@node.example.test", "relative"} {
		if _, err := NewClient(ClientOptions{NodeID: session.NodeID, URL: endpoint, CallTimeout: time.Second, TLS: testClientTLS(t)}); err == nil {
			t.Fatal("接受无效端点", endpoint)
		}
	}
	node := &testNode{fail: &computer.Failure{Code: "resource_conflict", IsPermanent: true}}
	_, err := testOperations(node).invoke(context.Background(), "ensure", testInputs(session)["ensure"])
	var failure *computer.Failure
	if !errors.As(err, &failure) || !failure.IsPermanent {
		t.Fatal("业务失败分类丢失")
	}
}

func TestNodeReservedIdentitiesAndEndpoint(t *testing.T) {
	session := testSession()
	if _, err := NewClient(ClientOptions{NodeID: "01k9ax6vsmkdbz049g41ft4whw", URL: "https://node.example.test", CallTimeout: time.Second, TLS: testClientTLS(t)}); err == nil {
		t.Fatal("非规范节点身份形成另一个控制记录")
	}
	if _, err := NewClient(ClientOptions{NodeID: session.NodeID, URL: "https://node.example.test", CallTimeout: time.Second}); err == nil {
		t.Fatal("缺少客户端证书仍被接受")
	}
	if _, err := ParseFingerprints([]string{strings.Repeat("A", 64)}); err == nil {
		t.Fatal("接受大写指纹")
	}
	if _, err := ParseFingerprints([]string{strings.Repeat("a", 64), strings.Repeat("a", 64)}); err == nil {
		t.Fatal("接受重复指纹")
	}
}

// testClientTLS 生成一次性自签证书，供客户端配置校验使用；不写入磁盘也不复用。
func testClientTLS(t *testing.T) *tls.Config {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "tombot-api-test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	encoded, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(encoded)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(leaf.Raw)
	fingerprints, err := ParseFingerprints([]string{hex.EncodeToString(fingerprint[:])})
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := ClientTLS(tls.Certificate{Certificate: [][]byte{encoded}, PrivateKey: private, Leaf: leaf}, fingerprints)
	if err != nil {
		t.Fatal(err)
	}
	return configuration
}
