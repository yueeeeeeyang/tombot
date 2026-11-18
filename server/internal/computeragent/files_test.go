package computeragent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

func TestFileTransferBinaryRoundTrip(t *testing.T) {
	fixture := newFixture(t)
	content := bytes.Repeat([]byte{0, 255, 13, 10}, 20000)
	digest := sha256.Sum256(content)
	var saved []byte
	writes := 0
	execute := func(_ context.Context, action, path string, size int, hash string, input []byte) ([]byte, error) {
		if path != "Library/report.pdf" {
			t.Fatal("路径未规范化", path)
		}
		if action == "read" {
			return bytes.Clone(saved), nil
		}
		if size != len(content) || hash != hex.EncodeToString(digest[:]) {
			t.Fatal("文件元数据不符")
		}
		writes++
		saved = bytes.Clone(input)
		return nil, nil
	}
	store := &fileTransfers{}
	request := computer.FileRequest{Session: fixture.command.Session, OperationID: ids.New(), Action: "import_start", Path: "/workspace/Library/report.pdf", Size: len(content), SHA256: hex.EncodeToString(digest[:])}
	call := func(request computer.FileRequest) computer.FileReceipt {
		t.Helper()
		receipt, err := store.transfer(t.Context(), request, execute)
		if err != nil || receipt.Validate(request) != nil {
			t.Fatal("文件传输失败", request.Action, err, receipt.Validate(request))
		}
		return receipt
	}
	call(request)
	if _, err := store.transfer(t.Context(), request, execute); err == nil {
		t.Fatal("启动被重放")
	}
	request.Action, request.Path, request.Size, request.SHA256 = "import_chunk", "", 0, ""
	for offset := 0; offset < len(content); offset += computer.MaximumFileChunkBytes {
		request.Offset, request.Bytes = offset, content[offset:min(offset+computer.MaximumFileChunkBytes, len(content))]
		call(request)
		if _, err := store.transfer(t.Context(), request, execute); err == nil {
			t.Fatal("分块被重放")
		}
	}
	request.Action, request.Offset, request.Bytes = "import_commit", 0, nil
	call(request)
	if _, err := store.transfer(t.Context(), request, execute); err == nil || writes != 1 {
		t.Fatal("提交被重放", writes)
	}
	request.Action = "close"
	call(request)
	call(request)
	if store.reserved != 0 || !bytes.Equal(saved, content) {
		t.Fatal("字节不一致或暂存未释放")
	}
	request.OperationID, request.Action, request.Path = ids.New(), "export_start", "/workspace/Library/report.pdf"
	first := call(request)
	request.Action, request.Path = "export_chunk", ""
	var exported []byte
	for request.Offset = 0; request.Offset < first.Size; request.Offset += computer.MaximumFileChunkBytes {
		receipt := call(request)
		exported = append(exported, receipt.Bytes...)
	}
	if !bytes.Equal(exported, content) {
		t.Fatal("导出损坏")
	}
	store.close()
	if _, err := store.transfer(t.Context(), request, execute); err == nil {
		t.Fatal("关闭后仍可读取")
	}
}

func TestFileTransferFailuresAndOwnership(t *testing.T) {
	fixture := newFixture(t)
	for _, mode := range []string{"摘要错误", "写入失败", "过大", "不同桌面", "取消"} {
		t.Run(mode, func(t *testing.T) {
			store := &fileTransfers{}
			defer store.close()
			digest := sha256.Sum256(nil)
			request := computer.FileRequest{Session: fixture.command.Session, OperationID: ids.New(), Action: "import_start", Path: "/workspace/report", SHA256: hex.EncodeToString(digest[:])}
			calls := 0
			execute := func(context.Context, string, string, int, string, []byte) ([]byte, error) {
				calls++
				return nil, errors.New("写入失败")
			}
			if mode == "摘要错误" {
				request.SHA256 = hex.EncodeToString(make([]byte, 32))
			}
			if mode == "过大" {
				request.Size = computer.MaximumFileBytes + 1
			}
			ctx := t.Context()
			if mode == "取消" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			_, err := store.transfer(ctx, request, execute)
			if mode == "过大" || mode == "取消" {
				if err == nil || calls != 0 {
					t.Fatal("无效请求仍执行")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			request.Action, request.Path, request.SHA256 = "import_commit", "", ""
			if mode == "不同桌面" {
				request.Session.FencingToken++
			}
			if _, err = store.transfer(ctx, request, execute); err == nil {
				t.Fatal("失败操作被接受")
			}
			if _, err = store.transfer(ctx, request, execute); err == nil {
				t.Fatal("失败提交被重放")
			}
			if mode == "写入失败" && calls != 1 || mode != "写入失败" && calls != 0 {
				t.Fatal("执行次数错误", calls)
			}
		})
	}
}

func TestFileTransferRejectsTraversal(t *testing.T) {
	for _, path := range []string{"", ".", "..", "../file", "/etc/passwd", "/workspace/../other", "a//b", "a/./b", "a\\b", "a\x00b", "Downloads/incomplete.crdownload"} {
		if _, err := computer.FilePath(path); err == nil {
			t.Fatal("接受了路径", path)
		}
	}
}
