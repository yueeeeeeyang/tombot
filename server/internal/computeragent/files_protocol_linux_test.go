//go:build linux

package computeragent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
)

type recordingFileScope struct {
	*fakeScope
	transfers fileTransfers
	content   []byte
}

func (scope *recordingFileScope) TransferFile(ctx context.Context, reference processgroup.Reference, request computer.FileRequest, membership, revocation *os.File) (computer.FileReceipt, error) {
	if reference.Session != request.Session || membership == nil || revocation == nil {
		return computer.FileReceipt{}, errors.New("测试文件授权缺失")
	}
	return scope.transfers.transfer(ctx, request, func(context.Context, string, string, int, string, []byte) ([]byte, error) {
		return bytes.Clone(scope.content), nil
	})
}

func TestKernelFileProtocol(t *testing.T) {
	f := newFixture(t)
	if _, err := f.controller.Open(t.Context(), f.command, f.reference, f.files(t)); err != nil {
		t.Fatal(err)
	}
	scope := &recordingFileScope{fakeScope: f.factory.scopes[0], content: bytes.Repeat([]byte{0, 255}, computer.MaximumFileChunkBytes)}
	f.guard.scope = scope
	defer scope.transfers.close()
	server, err := NewServer(f.controller, newTestProbeController(t, f), sameTestProcess, 2*time.Second, 2, f.guard.logger)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 1)
	client, err := NewClient(func(context.Context) (*net.UnixConn, error) {
		left, right := protocolPair(t)
		go func() {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			results <- server.serveConnection(ctx, right)
		}()
		return left, nil
	}, sameTestProcess, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	request := computer.FileRequest{Session: f.command.Session, OperationID: ids.New(), Action: "export_start", Path: "/workspace/result.bin"}
	for _, action := range []string{"export_start", "export_chunk", "close"} {
		request.Action = action
		if action != "export_start" {
			request.Path = ""
		}
		receipt, err := client.TransferFile(t.Context(), request)
		if err != nil || receipt.Validate(request) != nil {
			t.Fatal(action, err, receipt.Validate(request))
		}
		if err := <-results; err != nil {
			t.Fatal(err)
		}
		if action == "export_chunk" && !bytes.Equal(receipt.Bytes, scope.content[:computer.MaximumFileChunkBytes]) {
			t.Fatal("协议损坏二进制内容")
		}
	}
	request.Session.FencingToken++
	if _, err := client.TransferFile(t.Context(), request); err == nil {
		t.Fatal("旧桌面授权被接受")
	}
	if err := <-results; err == nil {
		t.Fatal("越权未返回错误")
	}
}

func TestFileOutputLimitCannotBeBypassedByCopy(t *testing.T) {
	output := &boundedFileOutput{}
	if _, err := io.Copy(output, bytes.NewReader(make([]byte, computer.MaximumFileBytes+1))); err == nil || len(output.Bytes()) > computer.MaximumFileBytes {
		t.Fatal("输出绕过上限")
	}
}
