//go:build nodeintegration

package nodeapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

// testCertificatePair 生成一次性自签证书；信任来自指纹而不是证书链，因此不需要 CA。
func testCertificatePair(t *testing.T, usage x509.ExtKeyUsage, serial int64) (tls.Certificate, Fingerprint) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "tombot-node-test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private, Leaf: leaf}, fingerprintOf(leaf.Raw)
}

// testEndpoint 启动真实 TLS 监听，返回地址与两端配置。
type testEndpoint struct {
	address       string
	serverTLS     *tls.Config
	clientTLS     *tls.Config
	serverFinger  Fingerprint
	clientFinger  Fingerprint
	foreignClient *tls.Config
}

func testEndpointConfig(t *testing.T) *testEndpoint {
	t.Helper()
	serverCertificate, serverFinger := testCertificatePair(t, x509.ExtKeyUsageServerAuth, 1)
	clientCertificate, clientFinger := testCertificatePair(t, x509.ExtKeyUsageClientAuth, 2)
	_, foreignFinger := testCertificatePair(t, x509.ExtKeyUsageClientAuth, 3)
	foreignCertificate, _ := testCertificatePair(t, x509.ExtKeyUsageClientAuth, 3)
	serverTLS, err := ServerTLS(serverCertificate, []Fingerprint{clientFinger})
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := ClientTLS(clientCertificate, []Fingerprint{serverFinger})
	if err != nil {
		t.Fatal(err)
	}
	foreignTLS, err := ClientTLS(foreignCertificate, []Fingerprint{serverFinger})
	if err != nil {
		t.Fatal(err)
	}
	if foreignFinger == clientFinger {
		t.Fatal("测试证书未区分身份")
	}
	return &testEndpoint{serverTLS: serverTLS, clientTLS: clientTLS, serverFinger: serverFinger, clientFinger: clientFinger, foreignClient: foreignTLS}
}

// startNode 启动节点服务并返回同一套证书的端点；监听端口由内核分配，测试之间互不冲突。
func startNode(t *testing.T, node *testNode, options ServerOptions) (*Server, string, *testEndpoint) {
	t.Helper()
	endpoint := testEndpointConfig(t)
	server, err := NewServer(testOperations(node), options, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := Listen("127.0.0.1:0", endpoint.serverTLS, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("节点服务没有有界停止")
		}
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	return server, "https://" + listener.Addr().String(), endpoint
}

func testClient(t *testing.T, nodeID, address string, configuration *tls.Config) *Client {
	t.Helper()
	client, err := NewClient(ClientOptions{NodeID: nodeID, URL: address, CallTimeout: time.Second, TLS: configuration})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.CloseIdle(); err != nil {
			t.Error(err)
		}
	})
	return client
}

func TestNodeClientFullProtocol(t *testing.T) {
	session := testSession()
	node := &testNode{}
	_, address, endpoint := startNode(t, node, testOptions(session.NodeID))
	client := testClient(t, session.NodeID, address, endpoint.clientTLS)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	inputs := testInputs(session)
	for _, action := range []string{"heartbeat", "inspect", "ensure", "remove", "open", "renew", "close", "start", "read", "write", "resize", "desktop_start", "desktop_status", "desktop_capture", "desktop_image_read", "desktop_input", "agent_start", "agent_write", "agent_read"} {
		input := inputs[action]
		var err error
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
			if err == nil && !bytes.Equal(output.Bytes, []byte{0, 1, 255}) {
				err = errors.New("二进制输出改变")
			}
		}
		if err != nil {
			t.Fatal(action, err)
		}
	}
	if node.calls.Load() != 19 {
		t.Fatal("没有完成全部受限动作", node.calls.Load())
	}
}

// TestNodeRejectsUnlistedClientCertificate 验证指纹不在允许列表的客户端在握手期就被拒绝，
// 请求不会到达节点动作。
func TestNodeRejectsUnlistedClientCertificate(t *testing.T) {
	session := testSession()
	node := &testNode{}
	_, address, endpoint := startNode(t, node, testOptions(session.NodeID))
	client := testClient(t, session.NodeID, address, endpoint.foreignClient)
	inputs := testInputs(session)
	if _, err := client.Ensure(context.Background(), inputs["ensure"].(computer.Specification)); err == nil {
		t.Fatal("未授权客户端执行了节点动作")
	}
	if node.calls.Load() != 0 {
		t.Fatal("握手失败的请求进入了节点", node.calls.Load())
	}
}

func TestNodeKnownAndUnknownFailure(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure error
	}{
		{"confirmed", &computer.Failure{Code: "resource_conflict", IsPermanent: true}},
		{"unknown", errors.New("raw-host-output")},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := testSession()
			node := &testNode{fail: test.failure}
			_, address, endpoint := startNode(t, node, testOptions(session.NodeID))
			client := testClient(t, session.NodeID, address, endpoint.clientTLS)
			_, err := client.Ensure(context.Background(), testInputs(session)["ensure"].(computer.Specification))
			var failure *computer.Failure
			switch test.name {
			case "confirmed":
				if !errors.As(err, &failure) || failure.Code != "resource_conflict" || !failure.IsPermanent {
					t.Fatal("确认的失败分类丢失", err)
				}
			default:
				if err == nil || bytes.Contains([]byte(err.Error()), []byte("raw-host-output")) {
					t.Fatal("未知调用伪成功或泄露原始错误", err)
				}
			}
			if node.calls.Load() != 1 {
				t.Fatal("失败动作被传输层重发", node.calls.Load())
			}
		})
	}
}

// post tampered requests directly; 这些请求不经过客户端校验，用来固定服务端的拒绝边界。
func post(t *testing.T, address string, configuration *tls.Config, body []byte, path, method string) int {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: configuration}, Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	request, err := http.NewRequest(method, address+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode
}

func TestNodeRejectsMalformedRequests(t *testing.T) {
	session := testSession()
	node := &testNode{}
	_, address, endpoint := startNode(t, node, testOptions(session.NodeID))
	command := testInputs(session)["open"].(computer.SessionCommand)
	encoded, err := agentchannel.Encode(command)
	if err != nil {
		t.Fatal(err)
	}
	lease := int64(500)
	base := func() request {
		return request{RequestID: ids.New(), NodeID: session.NodeID, Action: "open", RemainingMillis: 500, LeaseRemainMillis: &lease, Input: encoded}
	}
	encode := func(value request) []byte {
		body, err := agentchannel.Encode(value)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	for _, test := range []struct {
		name   string
		body   []byte
		path   string
		method string
	}{
		{"错误节点", func() []byte { value := base(); value.NodeID = ids.New(); return encode(value) }(), "/v2/actions", http.MethodPost},
		{"缺少请求标识", func() []byte { value := base(); value.RequestID = ""; return encode(value) }(), "/v2/actions", http.MethodPost},
		{"零剩余时长", func() []byte { value := base(); value.RemainingMillis = 0; return encode(value) }(), "/v2/actions", http.MethodPost},
		{"超过调用预算", func() []byte { value := base(); value.RemainingMillis = 120000; return encode(value) }(), "/v2/actions", http.MethodPost},
		{"缺少租约", func() []byte { value := base(); value.LeaseRemainMillis = nil; return encode(value) }(), "/v2/actions", http.MethodPost},
		{"租约超过动作寿命", func() []byte {
			value := base()
			oversized := int64(60000)
			value.LeaseRemainMillis = &oversized
			return encode(value)
		}(), "/v2/actions", http.MethodPost},
		{"多余租约", func() []byte { value := base(); value.Action = "close"; return encode(value) }(), "/v2/actions", http.MethodPost},
		{"未知动作", func() []byte { value := base(); value.Action = "docker_exec"; return encode(value) }(), "/v2/actions", http.MethodPost},
		{"尾随对象", append(encode(base()), []byte(` {}`)...), "/v2/actions", http.MethodPost},
		{"错误路径", encode(base()), "/v2/other", http.MethodPost},
		{"错误方法", encode(base()), "/v2/actions", http.MethodGet},
	} {
		t.Run(test.name, func(t *testing.T) {
			if status := post(t, address, endpoint.clientTLS, test.body, test.path, test.method); status == http.StatusOK {
				t.Fatal("无效请求被接受")
			}
		})
	}
	if node.calls.Load() != 0 {
		t.Fatal("无效请求进入了节点操作", node.calls.Load())
	}
}

// TestNodeLeaseTransitShortens 验证租约在传输前按剩余时长计算，节点按收包时刻重建后不会更长。
func TestNodeLeaseTransitShortens(t *testing.T) {
	session := testSession()
	node := &testNode{}
	_, address, endpoint := startNode(t, node, testOptions(session.NodeID))
	client := testClient(t, session.NodeID, address, endpoint.clientTLS)
	lifetime := 3 * time.Second
	before := time.Now().Add(lifetime)
	command := testInputs(session)["open"].(computer.SessionCommand)
	command.Lifetime = lifetime
	if _, err := client.Open(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	after := time.Now().Add(lifetime)
	if node.deadline.After(after) || node.deadline.Before(before) {
		t.Fatal("租约期限未按发送时剩余时长传递", node.deadline)
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

func TestNodeCapacityAndCancellation(t *testing.T) {
	session := testSession()
	node := &testNode{}
	entered := make(chan struct{})
	operations := testOperations(node)
	operations.Computers = blockedComputer{node, entered}
	options := testOptions(session.NodeID)
	options.MaximumConnections = 1
	server, err := NewServer(operations, options, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	endpoint := testEndpointConfig(t)
	listener, err := Listen("127.0.0.1:0", endpoint.serverTLS, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	client := testClient(t, session.NodeID, "https://"+listener.Addr().String(), endpoint.clientTLS)
	input := testInputs(session)["ensure"].(computer.Specification)
	first := make(chan error, 1)
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
		t.Error("容量上限未阻止第二次操作", node.calls.Load())
	}
}

type unfinishedComputer struct {
	*testNode
	entered chan struct{}
	release chan struct{}
}

func (node unfinishedComputer) Ensure(_ context.Context, _ computer.Specification) (computer.Observation, error) {
	close(node.entered)
	<-node.release
	return computer.Observation{}, errors.New("在途动作未收尾")
}

func TestNodeUnfinishedOperationRequiresExit(t *testing.T) {
	session := testSession()
	node := unfinishedComputer{testNode: &testNode{}, entered: make(chan struct{}), release: make(chan struct{})}
	operations := testOperations(node.testNode)
	operations.Computers = node
	server, err := NewServer(operations, testOptions(session.NodeID), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	// 构造后收紧退出预算：本用例只验证在途动作未收尾时进程必须退出。
	server.options.ShutdownTimeout = 20 * time.Millisecond
	endpoint := testEndpointConfig(t)
	listener, err := Listen("127.0.0.1:0", endpoint.serverTLS, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	client := testClient(t, session.NodeID, "https://"+listener.Addr().String(), endpoint.clientTLS)
	response := make(chan error, 1)
	go func() {
		_, err := client.Ensure(context.Background(), testInputs(session)["ensure"].(computer.Specification))
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
	case <-time.After(3 * time.Second):
		t.Fatal("节点未有界退出")
	}
}

// TestNodeClientRejectsUncertainResponses 用伪造服务端固定客户端的校验边界：
// 任何无法核对身份或内容的响应都必须按结果未知处理，且只发送一次。
func TestNodeClientRejectsUncertainResponses(t *testing.T) {
	for _, mode := range []string{"错误请求标识", "错误节点", "错误动作", "错误业务归属", "双重结果", "过大退出码", "非 JSON", "状态码失败"} {
		t.Run(mode, func(t *testing.T) {
			session := testSession()
			endpoint := testEndpointConfig(t)
			var received atomic.Int64
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received.Add(1)
				body, err := io.ReadAll(io.LimitReader(r.Body, agentchannel.MaximumBytes))
				if err != nil {
					t.Error(err)
					return
				}
				var content request
				if err := agentchannel.Decode(body, &content); err != nil {
					t.Error(err)
					return
				}
				if mode == "状态码失败" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if mode == "非 JSON" {
					w.WriteHeader(http.StatusOK)
					if _, err := w.Write([]byte("not-json")); err != nil {
						t.Error(err)
					}
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
					t.Error(err)
					return
				}
				result := response{RequestID: content.RequestID, NodeID: session.NodeID, Action: "start", Result: encoded}
				switch mode {
				case "错误请求标识":
					result.RequestID = ids.New()
				case "错误节点":
					result.NodeID = ids.New()
				case "错误动作":
					result.Action = "remove"
				case "双重结果":
					result.Failure = &computer.Failure{Code: "node_operation_unknown"}
				}
				body, err = agentchannel.Encode(result)
				if err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				if _, err := w.Write(body); err != nil {
					t.Error(err)
				}
			})
			serverTLS := endpoint.serverTLS.Clone()
			serverTLS.NextProtos = nil
			raw, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			httptest := &http.Server{Handler: handler, TLSConfig: serverTLS}
			go func() { _ = httptest.ServeTLS(raw, "", "") }()
			t.Cleanup(func() { _ = httptest.Close() })
			client := testClient(t, session.NodeID, "https://"+raw.Addr().String(), endpoint.clientTLS)
			if _, err := client.StartTerminal(context.Background(), testInputs(session)["start"].(computer.TerminalStart)); err == nil {
				t.Fatal("伪造响应被接受", mode)
			}
			if received.Load() != 1 {
				t.Fatal("请求被重发", mode, received.Load())
			}
		})
	}
}

// TestNodeListenRequiresExplicitPublicAddress 固定监听保护：默认只允许私网与回环。
func TestNodeListenRequiresExplicitPublicAddress(t *testing.T) {
	endpoint := testEndpointConfig(t)
	if _, err := Listen("8.8.8.8:9443", endpoint.serverTLS, false); err == nil {
		t.Fatal("未显式允许时监听公网地址")
	}
	if _, err := Listen("127.0.0.1:0", endpoint.serverTLS, false); err != nil {
		t.Fatal("回环地址被拒绝", err)
	}
	listener, err := Listen("127.0.0.1:0", endpoint.serverTLS, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Error(err)
	}
}

// TestNodeConfigRejectsMissingCertificates 固定构造期校验，避免运行期才发现配置缺项。
func TestNodeConfigRejectsMissingCertificates(t *testing.T) {
	session := testSession()
	endpoint := testEndpointConfig(t)
	if _, err := Listen("127.0.0.1:0", nil, false); err == nil {
		t.Fatal("缺少 TLS 配置仍监听")
	}
	legacy := endpoint.serverTLS.Clone()
	legacy.MinVersion = tls.VersionTLS12
	if _, err := Listen("127.0.0.1:0", legacy, false); err == nil {
		t.Fatal("接受低于 TLS 1.3 的配置")
	}
	if _, err := NewServer(testOperations(&testNode{}), ServerOptions{NodeID: session.NodeID, CallTimeout: time.Second, ShutdownTimeout: time.Second, MaximumConnections: 1}, testLogger()); err != nil {
		t.Fatal(err)
	}
}

// TestNodeRequestIdentityBinding 验证请求体中的节点标识与证书身份必须同时成立。
func TestNodeRequestIdentityBinding(t *testing.T) {
	session := testSession()
	node := &testNode{}
	_, address, endpoint := startNode(t, node, testOptions(session.NodeID))
	client := testClient(t, ids.New(), address, endpoint.clientTLS)
	_, err := client.Heartbeat(context.Background(), computer.NodeQuery{NodeID: ids.New()})
	if err == nil {
		t.Fatal("其他节点标识被接受")
	}
	if node.calls.Load() != 0 {
		t.Fatal("错误节点调用了操作")
	}
}

var _ = json.Marshal
var _ = sha256.Sum256
var _ = hex.EncodeToString
