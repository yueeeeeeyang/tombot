//go:build linux && nodeintegration && !cgo

package nodeapp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodeapi"
)

type entryFixture struct {
	configuration Config
	path          string
	reads         atomic.Int32
	// 测试客户端证书由父测试保管，子进程通过临时文件接收。
	clientCertificate []byte
	clientPrivateKey  []byte
	serverFingerprint string
	address           string
}

// nodeTestCertificate 生成一次性自签证书并返回 PEM 与指纹；信任来自指纹而不是证书链。
func nodeTestCertificate(t *testing.T, usage x509.ExtKeyUsage, serial int64) (certificate, key []byte, fingerprint string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "tombot-entry-test"},
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
	encoded, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(leaf.Raw)
	certificate = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	key = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
	return certificate, key, hex.EncodeToString(digest[:])
}

// freePort 先占用再释放端口，让节点配置使用固定端口；测试之间由内核保证不冲突。
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// newEntryFixture 使用真实组件构造器和一次性 Docker HTTP 替身；不写宿主 cgroup 或挂载磁盘。
func newEntryFixture(t *testing.T) *entryFixture {
	t.Helper()
	if os.Geteuid() != 0 || os.Getenv("TOM_NODE_ENTRY_TEST") != "1" {
		t.Fatal("节点入口集成测试要求专用 Linux root 容器")
	}
	directory, err := os.MkdirTemp("/run", "node-entry-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0711); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	c := testConfig()
	c.Listen = "127.0.0.1:" + strconv.Itoa(freePort(t))
	c.Runtime.Socket = filepath.Join(directory, "docker.sock")
	c.Runtime.StateDirectory = filepath.Join(directory, "runtime")
	c.Runtime.StorageDirectory = filepath.Join(directory, "workspaces")
	c.Storage.StateDirectory = filepath.Join(directory, "storage")
	for _, path := range []string{c.Runtime.StateDirectory, c.Runtime.StorageDirectory, c.Storage.StateDirectory} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// 控制证书放在独立私有目录，节点只接受配置中的客户端指纹。
	private := filepath.Join(directory, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	c.Control.CertificateFile = filepath.Join(private, "node-certificate.pem")
	c.Control.PrivateKeyFile = filepath.Join(private, "node.key")
	serverCertificate, serverKey, serverFingerprint := nodeTestCertificate(t, x509.ExtKeyUsageServerAuth, 1)
	clientCertificate, clientKey, clientFingerprint := nodeTestCertificate(t, x509.ExtKeyUsageClientAuth, 2)
	c.Control.ClientFingerprints = []string{clientFingerprint}
	if err := os.WriteFile(c.Control.CertificateFile, serverCertificate, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Control.PrivateKeyFile, serverKey, 0600); err != nil {
		t.Fatal(err)
	}
	f := &entryFixture{configuration: c, path: filepath.Join(directory, "node.json"), clientCertificate: clientCertificate, clientPrivateKey: clientKey, serverFingerprint: serverFingerprint, address: "https://" + c.Listen}
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", c.Runtime.Socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(c.Runtime.Socket, 0660); err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && (strings.HasPrefix(r.URL.Path, "/v1.47/volumes/") || strings.HasPrefix(r.URL.Path, "/v1.47/containers/")) {
			f.reads.Add(1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		t.Error("空节点发生非只读 Docker 操作", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	})}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	return f
}

// assertReleased 重新绑定同一端口，确认节点退出后控制入口已经释放。
func (f *entryFixture) assertReleased(t *testing.T) {
	t.Helper()
	listener, err := net.Listen("tcp", f.configuration.Listen)
	if err != nil {
		t.Fatal("控制端口未释放", err)
	}
	if err := listener.Close(); err != nil {
		t.Error(err)
	}
}

func (f *entryFixture) client(t *testing.T) {
	t.Helper()
	user := ids.New()
	specification := computer.Specification{OperationID: ids.New(), UserID: user, ComputerID: ids.New(), NodeID: f.configuration.NodeID, ImageReference: "example.invalid/tombot@sha256:" + strings.Repeat("a", 64), VolumeReference: "tombot-user-" + user, Generation: 1, FencingToken: 1, Attempt: 1, LeaseExpiresAt: time.Now().Add(time.Minute), Resources: computer.Resources{CPUMillis: 1000, MemoryBytes: 512 << 20, DiskBytes: 2 << 30, ProcessLimit: 64}}
	encoded, err := json.Marshal(specification)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// 客户端证书写入子进程可读的临时文件；它只是一次性测试凭据，随临时目录清理。
	certificatePath := filepath.Join(filepath.Dir(f.path), "client-certificate.pem")
	keyPath := filepath.Join(filepath.Dir(f.path), "client.key")
	if err := os.WriteFile(certificatePath, f.clientCertificate, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, f.clientPrivateKey, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestLinuxEntryClientHelper$")
	command.Env = []string{"PATH=/usr/bin:/bin", "TOM_ENTRY_CLIENT=" + string(encoded), "TOM_ENTRY_ADDRESS=" + f.address, "TOM_ENTRY_CERTIFICATE=" + certificatePath, "TOM_ENTRY_KEY=" + keyPath, "TOM_ENTRY_SERVER_FINGERPRINT=" + f.serverFingerprint}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("独立客户端失败：%v %s", err, output)
	}
}

// entryTestClient 用 PEM 文件构造生产客户端，验证的是真实 TLS 握手路径。
func entryTestClient(nodeID, address, certificatePath, keyPath, fingerprint string) (*nodeapi.Client, error) {
	certificatePEM, err := os.ReadFile(certificatePath)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		return nil, err
	}
	fingerprints, err := nodeapi.ParseFingerprints([]string{fingerprint})
	if err != nil {
		return nil, err
	}
	configuration, err := nodeapi.ClientTLS(pair, fingerprints)
	if err != nil {
		return nil, err
	}
	return nodeapi.NewClient(nodeapi.ClientOptions{NodeID: nodeID, URL: address, CallTimeout: time.Second, TLS: configuration})
}

func TestLinuxEntryClientHelper(t *testing.T) {
	encoded := os.Getenv("TOM_ENTRY_CLIENT")
	if encoded == "" {
		t.Skip("只由父测试以独立进程启动")
	}
	var specification computer.Specification
	if err := json.Unmarshal([]byte(encoded), &specification); err != nil {
		t.Fatal(err)
	}
	client, err := entryTestClient(specification.NodeID, os.Getenv("TOM_ENTRY_ADDRESS"), os.Getenv("TOM_ENTRY_CERTIFICATE"), os.Getenv("TOM_ENTRY_KEY"), os.Getenv("TOM_ENTRY_SERVER_FINGERPRINT"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := client.CloseIdle(); err != nil {
			t.Error(err)
		}
	}()
	heartbeat, err := client.Heartbeat(t.Context(), computer.NodeQuery{NodeID: specification.NodeID})
	if err != nil || heartbeat.Validate(computer.NodeQuery{NodeID: specification.NodeID}) != nil {
		t.Fatal("真实控制心跳无效", err)
	}
	observation, err := client.Inspect(t.Context(), specification)
	if err != nil || !observation.Matches(specification) || observation.State != "absent" || observation.Capabilities.Ready() {
		t.Fatal("空节点实际状态错误", observation, err)
	}
}

func TestLinuxEntryStartupAndRestart(t *testing.T) {
	f := newEntryFixture(t)
	configuration, err := LoadConfig(f.path)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		application, err := New(t.Context(), configuration, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- application.Serve(ctx, nil) }()
		f.client(t)
		duplicate, stop := context.WithTimeout(t.Context(), 50*time.Millisecond)
		other, err := New(duplicate, configuration, testLogger())
		stop()
		if err == nil {
			cancel()
			if closeErr := other.Close(t.Context()); closeErr != nil {
				t.Error(closeErr)
			}
			t.Fatal("重复入口取得节点控制权")
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("节点未有界退出")
		}
		f.assertReleased(t)
	}
	if f.reads.Load() < 4 {
		t.Fatal("没有真正向 Docker 协议对账")
	}
}

func TestLinuxEntryRejectsUnsafeConfigurationAndRecovery(t *testing.T) {
	f := newEntryFixture(t)
	if err := os.Chmod(f.path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(f.path); err == nil {
		t.Fatal("公开配置被接受")
	}
	if err := os.Chmod(f.path, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.configuration.Runtime.StateDirectory, ids.New()+".probe.json")
	if err := os.WriteFile(path, []byte(`{"version":999}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(t.Context(), f.configuration, testLogger()); err == nil {
		t.Fatal("损坏恢复记录未阻止启动")
	}
	f.assertReleased(t)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("失败删除了恢复证据", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	application, err := New(t.Context(), f.configuration, testLogger())
	if err != nil {
		t.Fatal("失败的构造器遗留锁", err)
	}
	if err := application.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxNodeBinaryNotifyAndSignal(t *testing.T) {
	f := newEntryFixture(t)
	executable := os.Getenv("TOM_NODE_BINARY")
	if executable == "" {
		t.Fatal("必须提供本次构建的正式节点入口")
	}
	notifyPath := filepath.Join(filepath.Dir(f.path), "notify.sock")
	notify, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: notifyPath, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := notify.Close(); err != nil {
			t.Error(err)
		}
	}()
	raw, err := notify.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var optionErr error
	if err := raw.Control(func(fd uintptr) { optionErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PASSCRED, 1) }); err != nil || optionErr != nil {
		t.Fatal(err, optionErr)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable)
	command.Env = []string{"PATH=/usr/bin:/bin", "TOM_NODE_CONFIG=" + f.path, "NOTIFY_SOCKET=" + notifyPath}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	finished := false
	defer func() {
		cancel()
		if finished {
			return
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("节点测试进程未退出")
		}
	}()
	if err := notify.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	message := make([]byte, 1024)
	control := make([]byte, unix.CmsgSpace(unix.SizeofUcred))
	count, controlCount, flags, _, err := notify.ReadMsgUnix(message, control)
	if err != nil || !strings.Contains(string(message[:count]), "READY=1") {
		t.Fatal("正式进程未确认启动", err)
	}
	if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
		t.Fatal("启动通知或身份被截断")
	}
	messages, err := unix.ParseSocketControlMessage(control[:controlCount])
	if err != nil || len(messages) != 1 {
		t.Fatal("启动通知缺少内核身份", err)
	}
	credentials, err := unix.ParseUnixCredentials(&messages[0])
	if err != nil || credentials.Pid != int32(command.Process.Pid) || credentials.Uid != 0 {
		t.Fatal("通知并非由节点主进程发送", err)
	}
	f.client(t)
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("正式进程 SIGTERM 未完成退出")
	}
	f.assertReleased(t)
}

func TestLinuxNodeBinaryRejectsBrowserRecoveryBeforeListening(t *testing.T) {
	f := newEntryFixture(t)
	executable := os.Getenv("TOM_NODE_BINARY")
	if executable == "" {
		t.Fatal("必须提供本次构建的正式节点入口")
	}
	path := filepath.Join(f.configuration.Runtime.StateDirectory, ids.New()+".browser.json")
	encoded := []byte(`{"version":999}`)
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable)
	command.Env = []string{"PATH=/usr/bin:/bin", "TOM_NODE_CONFIG=" + f.path}
	if err := command.Run(); err == nil || ctx.Err() != nil {
		t.Fatal("浏览器恢复失败未及时退出", err)
	}
	f.assertReleased(t)
	retained, err := os.ReadFile(path)
	if err != nil || string(retained) != string(encoded) {
		t.Fatal("错误记录未保留", err)
	}
	// 子进程退出必须释放节点锁；删除仅发生在本测试创建的损坏夹具中。
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	application, err := New(t.Context(), f.configuration, testLogger())
	if err != nil {
		t.Fatal("失败进程遗留节点锁", err)
	}
	if err := application.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}
