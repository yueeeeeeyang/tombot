package computeragent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
)

func TestDesktopImageCacheAndBinding(t *testing.T) {
	session := newFixture(t).command.Session
	var calls atomic.Int64
	done := make(chan error, 2)
	access := newDesktopAccess(session, func(context.Context) (net.Conn, error) {
		calls.Add(1)
		client, server := net.Pipe()
		go func() { done <- serveTestCapture(server, [4]byte{5, 6, 7, 0}) }()
		return client, nil
	})
	t.Cleanup(func() {
		if err := access.close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	request := computer.DesktopCapture{Session: session, Sequence: 1}
	first, err := access.capture(context.Background(), request)
	if err != nil || first.Validate(request) != nil {
		t.Fatal(first, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	second, err := access.capture(context.Background(), request)
	if err != nil || first != second || calls.Load() != 1 {
		t.Fatal("重复截图改变画面或重新连接", err)
	}
	var encoded []byte
	read := computer.DesktopImageRead{DesktopCapture: request, MaximumBytes: 3072}
	for read.Offset < first.Size {
		chunk, err := access.read(context.Background(), read)
		if err != nil || chunk.Validate(read) != nil {
			t.Fatal(err)
		}
		encoded = append(encoded, chunk.Bytes...)
		read.Offset += len(chunk.Bytes)
		// 外部修改返回块不能污染缓存。
		clear(chunk.Bytes)
	}
	digest := sha256.Sum256(encoded)
	if hex.EncodeToString(digest[:]) != first.SHA256 {
		t.Fatal("分块重组摘要不符")
	}
	read.Offset = 0
	chunk, err := access.read(context.Background(), read)
	if err != nil || !slices.Equal(chunk.Bytes, encoded[:len(chunk.Bytes)]) {
		t.Fatal("缓存被调用者改写", err)
	}
	wrong := request
	wrong.Session.FencingToken++
	if _, err := access.capture(context.Background(), wrong); err == nil {
		t.Fatal("其他代次复用了截图")
	}
	if calls.Load() != 1 {
		t.Fatal("错误归属仍连接桌面")
	}
	request.Sequence++
	if _, err := access.capture(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := access.read(context.Background(), read); err == nil {
		t.Fatal("旧画面分块被新画面替换后仍成功")
	}
	if err := access.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if access.png != nil {
		t.Fatal("关闭后未释放截图")
	}
	if _, err := access.capture(context.Background(), request); err == nil {
		t.Fatal("关闭后继续截图")
	}
}

func TestDesktopInputNeverReplaysUnknown(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "正常", true: "中途断连"}[fail], func(t *testing.T) {
			session := newFixture(t).command.Session
			var calls atomic.Int64
			done := make(chan error, 1)
			access := newDesktopAccess(session, func(context.Context) (net.Conn, error) {
				calls.Add(1)
				client, server := net.Pipe()
				go func() {
					err := writeTestHandshake(server, desktopHeader())
					if err == nil {
						length := 16
						if fail {
							length = 8
						}
						_, err = io.ReadFull(server, make([]byte, length))
					}
					done <- errors.Join(err, server.Close())
				}()
				return client, nil
			})
			t.Cleanup(func() {
				if err := access.close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			// 模型操作和参数拒绝也有持久序号，首个实际输入允许从更大的序号开始。
			request := computer.DesktopInput{Session: session, Sequence: 3, Events: []computer.DesktopEvent{{Kind: "key", Key: 'a', Down: true}, {Kind: "key", Key: 'a'}}}
			receipt, err := access.send(context.Background(), request)
			want := "sent"
			if fail {
				want = "uncertain"
			}
			if err != nil || receipt.State != want || receipt.Validate(request) != nil {
				t.Fatal(receipt, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			repeated, err := access.send(context.Background(), request)
			if err != nil || repeated != receipt || calls.Load() != 1 {
				t.Fatal("重复或未知输入被重放", err)
			}
			altered := request
			altered.Events = []computer.DesktopEvent{{Kind: "pointer"}}
			if _, err := access.send(context.Background(), altered); err == nil {
				t.Fatal("同序号换内容被接受")
			}
			request.Sequence--
			if _, err := access.send(context.Background(), request); err == nil {
				t.Fatal("迟到的旧输入被接受")
			}
			if fail {
				request.Sequence = 4
				if _, err := access.send(context.Background(), request); err == nil {
					t.Fatal("未知输入后继续产生副作用")
				}
			}
			if calls.Load() != 1 {
				t.Fatal("拒绝动作仍连接桌面")
			}
		})
	}
}

func TestDesktopRevocationCancelsBlockedCapture(t *testing.T) {
	session := newFixture(t).command.Session
	client, server := net.Pipe()
	started := make(chan struct{})
	access := newDesktopAccess(session, func(context.Context) (net.Conn, error) { close(started); return client, nil })
	done := make(chan error, 1)
	go func() {
		_, err := access.capture(context.Background(), computer.DesktopCapture{Session: session, Sequence: 1})
		done <- err
	}()
	<-started
	access.stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("撤销后仍返回画面")
		}
	case <-time.After(time.Second):
		t.Fatal("撤销等待截图读锁")
	}
	if err := errors.Join(access.close(context.Background()), server.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopCaptureFailureCannotChangeSameSequence(t *testing.T) {
	session := newFixture(t).command.Session
	calls := 0
	access := newDesktopAccess(session, func(context.Context) (net.Conn, error) { calls++; return nil, errors.New("测试连接失败") })
	defer access.stop()
	request := computer.DesktopCapture{Session: session, Sequence: 1}
	for range 2 {
		if _, err := access.capture(context.Background(), request); err == nil {
			t.Fatal("失败截图返回成功")
		}
	}
	if calls != 1 {
		t.Fatal("同序号的失败截图重新获取画面")
	}
	if _, err := access.read(context.Background(), computer.DesktopImageRead{DesktopCapture: request, MaximumBytes: 10}); err == nil {
		t.Fatal("失败截图被当作空画面")
	}
	request.Sequence++
	if _, err := access.capture(context.Background(), request); err == nil {
		t.Fatal("新截图返回虚假成功")
	}
	if calls != 2 {
		t.Fatal("新截图未单独尝试")
	}
}

type recordingDesktopIOScope struct {
	*fakeScope
	access *desktopAccess
	calls  int
}

func (scope *recordingDesktopIOScope) CaptureDesktop(ctx context.Context, request computer.DesktopCapture) (computer.DesktopImage, error) {
	scope.calls++
	return scope.access.capture(ctx, request)
}
func (scope *recordingDesktopIOScope) ReadDesktopImage(ctx context.Context, request computer.DesktopImageRead) (computer.DesktopImageChunk, error) {
	scope.calls++
	return scope.access.read(ctx, request)
}
func (scope *recordingDesktopIOScope) InputDesktop(ctx context.Context, request computer.DesktopInput) (computer.DesktopInputReceipt, error) {
	scope.calls++
	return scope.access.send(ctx, request)
}

func TestDesktopIOControllerAuthorization(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	session := fixture.command.Session
	input := computer.DesktopInput{Session: session, Sequence: 1, Events: []computer.DesktopEvent{{Kind: "pointer"}}}
	if _, err := fixture.controller.InputDesktop(ctx, input); err == nil {
		t.Fatal("未授权输入被接受")
	}
	if _, err := fixture.controller.Open(ctx, fixture.command, fixture.reference, fixture.files(t)); err != nil {
		t.Fatal(err)
	}
	access := newDesktopAccess(session, func(context.Context) (net.Conn, error) { return nil, errors.New("测试端点离线") })
	defer access.stop()
	scope := &recordingDesktopIOScope{fakeScope: fixture.factory.scopes[0], access: access}
	fixture.guard.scope = scope
	for _, change := range []func(*computer.ExecutionSession){func(s *computer.ExecutionSession) { s.FencingToken++ }, func(s *computer.ExecutionSession) { s.Generation++ }, func(s *computer.ExecutionSession) { s.BotID = s.RunID }, func(s *computer.ExecutionSession) { s.UserID = s.RunID }} {
		wrong := session
		change(&wrong)
		if _, err := fixture.controller.CaptureDesktop(ctx, computer.DesktopCapture{Session: wrong, Sequence: 1}); err == nil {
			t.Fatal("越权截图被接受")
		}
		if _, err := fixture.controller.ReadDesktopImage(ctx, computer.DesktopImageRead{DesktopCapture: computer.DesktopCapture{Session: wrong, Sequence: 1}, MaximumBytes: 10}); err == nil {
			t.Fatal("越权读取被接受")
		}
		invalid := input
		invalid.Session = wrong
		if _, err := fixture.controller.InputDesktop(ctx, invalid); err == nil {
			t.Fatal("越权输入被接受")
		}
	}
	if scope.calls != 0 {
		t.Fatal("越权仍进入执行器")
	}
	if receipt, err := fixture.controller.InputDesktop(ctx, input); err != nil || receipt.State != "uncertain" {
		t.Fatal(receipt, err)
	}
	if _, err := fixture.controller.Close(ctx, fixture.command); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.controller.InputDesktop(ctx, input); err == nil {
		t.Fatal("关闭后输入仍被接受")
	}
	if scope.calls != 1 {
		t.Fatal("关闭后仍进入执行器")
	}
}
