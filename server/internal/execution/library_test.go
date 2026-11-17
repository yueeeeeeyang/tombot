package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"strings"
	"sync"
	"testing"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/chatlibrary"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestChatLibrary(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("持久化幂等上传跨用户跨群隔离与事件", func(t *testing.T) {
		f := newFixture(t, cluster)
		group := f.group(t)
		other, _, creationError := f.catalog.CreateGroup(ctx, f.a, bots.CreateGroupRequest{ClientRequestID: ids.New(), GroupSpec: bots.GroupSpec{Name: "另一群", MemberIDs: []string{f.first.ID}}})
		if creationError != nil {
			t.Fatal(creationError)
		}
		request := chatlibrary.Upload{ClientRequestID: ids.New(), Name: "中文资料.txt", Content: []byte("共享内容")}
		file, err := f.service.UploadChatFile(ctx, f.a, group.ConversationID, request)
		if err != nil {
			t.Fatal(err)
		}
		again, err := f.service.UploadChatFile(ctx, f.a, group.ConversationID, request)
		if err != nil || again.ID != file.ID {
			t.Fatal(again, err)
		}
		downloaded, err := f.service.ChatFileContent(ctx, f.a, group.ConversationID, file.ID)
		if err != nil || !bytes.Equal(downloaded.Bytes, request.Content) {
			t.Fatal(downloaded, err)
		}
		hash := sha256.Sum256(request.Content)
		if file.SHA256 != hex.EncodeToString(hash[:]) {
			t.Fatal("摘要错误")
		}
		staged, err := f.service.ChatFiles(ctx, f.a, group.ConversationID, "", "")
		if err != nil || len(staged.Files) != 0 {
			t.Fatal("未发送文件进入资料库", staged, err)
		}
		if _, err = f.service.Send(ctx, f.a, group.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "分享资料", Mentions: []string{f.first.ID}, FileIDs: []string{request.ClientRequestID}}); err != nil {
			t.Fatal(err)
		}
		page, err := f.service.ChatFiles(ctx, f.a, group.ConversationID, "", "")
		if err != nil || len(page.Files) != 1 || page.UsedBytes != int64(len(request.Content)) {
			t.Fatal(page, err)
		}
		if _, err = f.service.ChatFiles(ctx, f.b, group.ConversationID, "", ""); !errors.Is(err, chatlibrary.ErrNotFound) {
			t.Fatal("跨用户泄漏", err)
		}
		if _, err = f.service.ChatFileContent(ctx, f.a, other.ConversationID, file.ID); !errors.Is(err, chatlibrary.ErrNotFound) {
			t.Fatal("跨群泄漏", err)
		}
		if err = f.tenant.Snapshot(ctx, f.b.UserID, func(tx *sql.Tx) error {
			for _, query := range []string{`SELECT count(*) FROM tombot.chat_files WHERE id=$1`, `SELECT count(*) FROM tombot.chat_file_contents WHERE file_id=$1`} {
				var count int
				if err := tx.QueryRowContext(ctx, query, file.ID).Scan(&count); err != nil {
					return err
				}
				if count != 0 {
					t.Error("数据库 RLS 泄漏其他用户文件")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		archived, err := f.catalog.SetGroupArchived(ctx, f.a, group.ID, group.Revision, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.service.ChatFileContent(ctx, f.a, group.ConversationID, file.ID); !errors.Is(err, chatlibrary.ErrNotFound) {
			t.Fatal("归档群仍可读取", err)
		}
		if _, err = f.catalog.SetGroupArchived(ctx, f.a, group.ID, archived.Revision, false); err != nil {
			t.Fatal(err)
		}
		request.Content = []byte("其他内容")
		if _, err = f.service.UploadChatFile(ctx, f.a, group.ConversationID, request); !errors.Is(err, chatlibrary.ErrConflict) {
			t.Fatal("复用幂等键", err)
		}
		var events int
		if err = f.pool.QueryRow(`SELECT count(*) FROM tombot.user_events WHERE user_id=$1 AND kind='chat.library.changed' AND resource_id=$2`, f.a.UserID, group.ConversationID).Scan(&events); err != nil || events != 1 {
			t.Fatal("事件未原子去重", events, err)
		}
	})
	t.Run("名称大小边界分页搜索及删除释放", func(t *testing.T) {
		f := newFixture(t, cluster)
		group := f.group(t)
		for _, name := range []string{"", "../资料", "a/b", "a\\b", "\x00", "资料\u202etxt", strings.Repeat("中", 81), " name "} {
			if _, err := f.service.UploadChatFile(ctx, f.a, group.ConversationID, chatlibrary.Upload{ClientRequestID: ids.New(), Name: name, Content: []byte{}}); !errors.Is(err, chatlibrary.ErrInput) {
				t.Fatal(name, err)
			}
		}
		if _, err := f.service.UploadChatFile(ctx, f.a, group.ConversationID, chatlibrary.Upload{ClientRequestID: ids.New(), Name: "too-big", Content: make([]byte, chatlibrary.MaxFileBytes+1)}); !errors.Is(err, chatlibrary.ErrInput) {
			t.Fatal(err)
		}
		for i := 0; i < 21; i++ {
			file, err := f.service.UploadChatFile(ctx, f.a, group.ConversationID, chatlibrary.Upload{ClientRequestID: ids.New(), Name: "100%_中文.txt", Content: []byte{}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.service.Send(ctx, f.a, group.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "资料", Mentions: []string{f.first.ID}, FileIDs: []string{file.ID}}); err != nil {
				t.Fatal(err)
			}
			lease := f.claim(t)
			f.start(t, lease)
			if _, err = f.service.Complete(ctx, lease, Outcome{Status: "failed", ErrorCode: "TEST_NOT_EXECUTED"}); err != nil {
				t.Fatal(err)
			}
		}
		first, err := f.service.ChatFiles(ctx, f.a, group.ConversationID, "", "%_")
		if err != nil || len(first.Files) != 20 || first.Next == "" {
			t.Fatal(first, err)
		}
		second, err := f.service.ChatFiles(ctx, f.a, group.ConversationID, first.Next, "%_")
		if err != nil || len(second.Files) != 1 || second.Next != "" {
			t.Fatal(second, err)
		}
		if second.Files[0].ID >= first.Next {
			t.Fatal("游标重复")
		}
		file := first.Files[0]
		renamed, err := f.service.RenameChatFile(ctx, f.a, group.ConversationID, file.ID, 1, "新版.txt")
		if err != nil || renamed.Revision != 2 {
			t.Fatal(renamed, err)
		}
		if _, err = f.service.RenameChatFile(ctx, f.a, group.ConversationID, file.ID, 1, "新版.txt"); err != nil {
			t.Fatal("重试改名不幂等", err)
		}
		if _, err = f.service.RenameChatFile(ctx, f.a, group.ConversationID, file.ID, 1, "覆盖.txt"); !errors.Is(err, chatlibrary.ErrConflict) {
			t.Fatal(err)
		}
		if err = f.service.DeleteChatFile(ctx, f.a, group.ConversationID, file.ID, 1); !errors.Is(err, chatlibrary.ErrConflict) {
			t.Fatal(err)
		}
		if err = f.service.DeleteChatFile(ctx, f.a, group.ConversationID, file.ID, 2); err != nil {
			t.Fatal(err)
		}
		if err = f.service.DeleteChatFile(ctx, f.a, group.ConversationID, file.ID, 2); err != nil {
			t.Fatal("重复删除失败", err)
		}
		if _, err = f.service.ChatFileContent(ctx, f.a, group.ConversationID, file.ID); !errors.Is(err, chatlibrary.ErrNotFound) {
			t.Fatal(err)
		}
		if _, err = f.service.UploadChatFile(ctx, f.a, group.ConversationID, chatlibrary.Upload{ClientRequestID: file.ID, Name: "100%_中文.txt", Content: []byte{}}); !errors.Is(err, chatlibrary.ErrConflict) {
			t.Fatal("旧请求复活文件", err)
		}
		var count int
		if err = f.pool.QueryRow(`SELECT count(*) FROM tombot.chat_file_contents WHERE user_id=$1 AND file_id=$2`, f.a.UserID, file.ID).Scan(&count); err != nil || count != 0 {
			t.Fatal("正文未释放", count, err)
		}
	})
	t.Run("容量边界删除后恢复及图片校验", func(t *testing.T) {
		f := newFixture(t, cluster)
		group := f.group(t)
		var oversized bytes.Buffer
		if err := png.Encode(&oversized, image.NewRGBA(image.Rect(0, 0, 8193, 1))); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.UploadChatFile(ctx, f.a, group.ConversationID, chatlibrary.Upload{ClientRequestID: ids.New(), Name: "大尺寸.png", Content: oversized.Bytes()}); !errors.Is(err, chatlibrary.ErrInput) {
			t.Fatal("图片尺寸边界未拒绝", err)
		}
		binary := make([]byte, chatlibrary.MaxFileBytes)
		var first chatlibrary.File
		for i := 0; i < chatlibrary.MaxUserBytes/chatlibrary.MaxFileBytes; i++ {
			file, err := f.service.UploadChatFile(ctx, f.a, group.ConversationID, chatlibrary.Upload{ClientRequestID: ids.New(), Name: "容量测试.bin", Content: binary})
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				first = file
			}
		}
		request := chatlibrary.Upload{ClientRequestID: ids.New(), Name: "超出容量.bin", Content: []byte{1}}
		if _, err := f.service.UploadChatFile(ctx, f.a, group.ConversationID, request); !errors.Is(err, chatlibrary.ErrCapacity) {
			t.Fatal("总容量未限制", err)
		}
		if err := f.service.DeleteChatFile(ctx, f.a, group.ConversationID, first.ID, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.UploadChatFile(ctx, f.a, group.ConversationID, request); err != nil {
			t.Fatal("删除未释放配额", err)
		}
	})
	t.Run("文件数量限制包含空文件", func(t *testing.T) {
		f := newFixture(t, cluster)
		group := f.group(t)
		err := f.service.libraryWrite(ctx, f.a, func(tx *sql.Tx) error {
			for i := 0; i < chatlibrary.MaxUserFiles; i++ {
				if _, err := chatlibrary.Save(ctx, tx, f.a.UserID, group.ConversationID, chatlibrary.Upload{ClientRequestID: ids.New(), Name: "空文件.txt", Content: []byte{}}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.service.UploadChatFile(ctx, f.a, group.ConversationID, chatlibrary.Upload{ClientRequestID: ids.New(), Name: "额外文件.txt", Content: []byte{}}); !errors.Is(err, chatlibrary.ErrCapacity) {
			t.Fatal("空文件绕过数量限制", err)
		}
	})
	t.Run("并发上传同一幂等键只保存一次", func(t *testing.T) {
		f := newFixture(t, cluster)
		group := f.group(t)
		request := chatlibrary.Upload{ClientRequestID: ids.New(), Name: "并发.txt", Content: []byte("内容")}
		var wait sync.WaitGroup
		failures := make(chan error, 4)
		for i := 0; i < 4; i++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				_, err := f.service.UploadChatFile(ctx, f.a, group.ConversationID, request)
				failures <- err
			}()
		}
		wait.Wait()
		close(failures)
		for err := range failures {
			if err != nil {
				t.Fatal(err)
			}
		}
		staged, err := f.service.ChatFiles(ctx, f.a, group.ConversationID, "", "")
		if err != nil || len(staged.Files) != 0 {
			t.Fatal("未发送文件进入资料库", staged, err)
		}
		if _, err = f.service.Send(ctx, f.a, group.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "分享资料", Mentions: []string{f.first.ID}, FileIDs: []string{request.ClientRequestID}}); err != nil {
			t.Fatal(err)
		}
		page, err := f.service.ChatFiles(ctx, f.a, group.ConversationID, "", "")
		if err != nil || len(page.Files) != 1 {
			t.Fatal(page, err)
		}
	})
	t.Run("群消息附件引用持久化及租户校验", func(t *testing.T) {
		f := newFixture(t, cluster)
		f.toolsModel(t)
		group := f.group(t)
		other, _, creationError := f.catalog.CreateGroup(ctx, f.a, bots.CreateGroupRequest{ClientRequestID: ids.New(), GroupSpec: bots.GroupSpec{Name: "另一群", MemberIDs: []string{f.first.ID}}})
		if creationError != nil {
			t.Fatal(creationError)
		}
		file, err := f.service.UploadChatFile(ctx, f.a, group.ConversationID, chatlibrary.Upload{ClientRequestID: ids.New(), Name: "需求.txt", Content: []byte("完整需求")})
		if err != nil {
			t.Fatal(err)
		}
		request := SendRequest{ClientRequestID: ids.New(), Content: "核对资料", Mentions: []string{f.first.ID}, FileIDs: []string{file.ID}}
		if _, err = f.service.Send(ctx, f.a, other.ConversationID, request); !errors.Is(err, chatlibrary.ErrNotFound) {
			t.Fatal("允许跨群附件", err)
		}
		sent, err := f.service.Send(ctx, f.a, group.ConversationID, request)
		if err != nil {
			t.Fatal(err)
		}
		history, err := f.service.Messages(ctx, f.a, group.ConversationID, 0, 20)
		if err != nil || len(history) != 1 || history[0].Blocks[1].ChatFile.ID != file.ID {
			t.Fatal(history, err)
		}
		model := messageForModel(history[0])
		encodedModel, marshalError := json.Marshal(model)
		if marshalError != nil {
			t.Fatal(marshalError)
		}
		if !strings.Contains(string(encodedModel), file.ID) {
			t.Fatal("模型缺少附件元数据")
		}
		if err = f.service.DeleteChatFile(ctx, f.a, group.ConversationID, file.ID, 1); err != nil {
			t.Fatal(err)
		}
		replay, err := f.service.Send(ctx, f.a, group.ConversationID, request)
		if err != nil || replay.MessageID != sent.MessageID {
			t.Fatal("响应重试未复用已发送消息", err)
		}
		request.ClientRequestID = ids.New()
		if _, err = f.service.Send(ctx, f.a, group.ConversationID, request); !errors.Is(err, chatlibrary.ErrNotFound) {
			t.Fatal("新消息引用已删除文件", err)
		}
	})
	t.Run("工具只能读取当前群文本并原子保存产物", func(t *testing.T) {
		f := newFixture(t, cluster)
		f.toolsModel(t)
		group := f.group(t)
		file, err := f.service.UploadChatFile(ctx, f.a, group.ConversationID, chatlibrary.Upload{ClientRequestID: ids.New(), Name: "资料.txt", Content: []byte(strings.Repeat("中文资料", 200))})
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.service.Send(ctx, f.a, group.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "阅读资料并生成说明", Mentions: []string{f.first.ID}, FileIDs: []string{file.ID}})
		if err != nil {
			t.Fatal(err)
		}
		lease := f.claim(t)
		f.start(t, lease)
		callTool := func(name string, arguments map[string]any) botagent.ToolResult {
			t.Helper()
			call := botagent.ToolCall{ID: ids.New(), Name: name, Arguments: memoryArguments(t, arguments)}
			encoded, err := json.Marshal(call)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(encoded)
			op, _, err := f.service.BeginOperation(ctx, lease, OperationRequest{StepKey: "library." + strings.ToLower(call.ID), Kind: "tool", ToolName: name, RequestDigest: digest[:]})
			if err != nil {
				t.Fatal(err)
			}
			for _, schema := range chatlibrary.Schemas() {
				if schema.Name == name {
					result, err := (chatFileTool{executor: f.service, schema: schema, maximum: 8 << 10}).Execute(ctx, Invocation{Lease: lease, OperationID: op.ID, Call: call})
					if err != nil {
						t.Fatal(err)
					}
					status, code := "succeeded", ""
					if result.IsError {
						status, code = "failed", "TOOL_FAILED"
					}
					if _, err = f.service.RecordOperation(ctx, lease, op.ID, OperationResult{Status: status, ErrorCode: code, Result: json.RawMessage(memoryArguments(t, result))}); err != nil {
						t.Fatal(err)
					}
					return result
				}
			}
			t.Fatal("工具不存在")
			return botagent.ToolResult{}
		}
		listed := callTool("chat_file_list", map[string]any{"before": "", "query": "资料"})
		if listed.IsError || !strings.Contains(listed.Content, file.ID) {
			t.Fatal(listed)
		}
		offset := 0
		var text strings.Builder
		for {
			result := callTool("chat_file_read", map[string]any{"id": file.ID, "offset": offset})
			if result.IsError {
				t.Fatal(result)
			}
			var chunk struct {
				Text string `json:"text"`
				Next *int   `json:"next_offset"`
			}
			if err = json.Unmarshal([]byte(result.Content), &chunk); err != nil {
				t.Fatal(err)
			}
			text.WriteString(chunk.Text)
			if chunk.Next == nil {
				break
			}
			if *chunk.Next <= offset {
				t.Fatal("游标未推进")
			}
			offset = *chunk.Next
		}
		if text.String() != strings.Repeat("中文资料", 200) {
			t.Fatal("分段文本不完整")
		}
		denied := callTool("chat_file_read", map[string]any{"id": ids.New(), "offset": 0})
		if !denied.IsError {
			t.Fatal(denied)
		}
		invalid := callTool("chat_file_read", map[string]any{"id": file.ID, "offset": 1})
		if !invalid.IsError {
			t.Fatal("允许从 UTF-8 续字节开始读取")
		}
		binary, err := f.service.UploadChatFile(ctx, f.a, group.ConversationID, chatlibrary.Upload{ClientRequestID: ids.New(), Name: "二进制.bin", Content: []byte{0, 1, 2}})
		if err != nil {
			t.Fatal(err)
		}
		unsupported := callTool("chat_file_read", map[string]any{"id": binary.ID, "offset": 0})
		if !unsupported.IsError || !strings.Contains(unsupported.Content, "UNSUPPORTED_FILE") {
			t.Fatal("二进制被伪装为可读文本")
		}
		saved := callTool("chat_file_create", map[string]any{"name": "说明.md", "content": "# 核对结果"})
		if saved.IsError {
			t.Fatal(saved)
		}
		page, err := f.service.ChatFiles(ctx, f.a, group.ConversationID, "", "说明")
		if err != nil || len(page.Files) != 1 {
			t.Fatal(page, err)
		}
		history, err := f.service.Messages(ctx, f.a, group.ConversationID, 0, 10)
		if err != nil || len(history) != 2 || history[0].Blocks[1].ChatFile == nil || history[0].Blocks[1].ChatFile.ID != page.Files[0].ID || history[0].ReplyTo == nil || history[0].ReplyTo.MessageID != history[1].ID {
			t.Fatal("群生成文件未发送或丢失提及引用", history, err)
		}
	})
}

func TestDirectChatLibrary(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	f := newFixture(t, cluster)
	f.toolsModel(t)
	conversation := f.first.ConversationID
	file, err := f.service.UploadChatFile(ctx, f.a, conversation, chatlibrary.Upload{ClientRequestID: ids.New(), Name: "用户需求.txt", Content: []byte("需求正文")})
	if err != nil {
		t.Fatal(err)
	}
	staged, err := f.service.ChatFiles(ctx, f.a, conversation, "", "")
	if err != nil || len(staged.Files) != 0 {
		t.Fatal("草稿附件不能进入资料库", staged, err)
	}
	request := SendRequest{ClientRequestID: ids.New(), Content: "阅读附件", FileIDs: []string{file.ID}}
	for _, other := range []string{f.second.ConversationID, f.foreign.ConversationID} {
		if _, err = f.service.Send(ctx, f.a, other, request); err == nil {
			t.Fatal("允许在其他单聊发送附件")
		}
	}
	for _, invalid := range [][]string{{file.ID, file.ID}, {file.ID, file.ID, file.ID, file.ID, file.ID}} {
		failed := request
		failed.ClientRequestID, failed.FileIDs = ids.New(), invalid
		if _, err = f.service.Send(ctx, f.a, conversation, failed); !errors.Is(err, ErrInvalidInput) {
			t.Fatal("重复或超限附件未拒绝", err)
		}
	}
	sent, err := f.service.Send(ctx, f.a, conversation, request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.service.Send(ctx, f.a, conversation, request)
	if err != nil || replay.MessageID != sent.MessageID {
		t.Fatal("重复发送产生新消息", err)
	}
	changed := request
	changed.FileIDs = nil
	if _, err = f.service.Send(ctx, f.a, conversation, changed); !errors.Is(err, ErrConflict) {
		t.Fatal("幂等键没有包含附件", err)
	}
	page, err := f.service.ChatFiles(ctx, f.a, conversation, "", "用户")
	if err != nil || len(page.Files) != 1 || page.Files[0].ID != file.ID {
		t.Fatal(page, err)
	}
	if _, err = f.service.ChatFileContent(ctx, f.b, conversation, file.ID); !errors.Is(err, chatlibrary.ErrNotFound) {
		t.Fatal("跨租户文件泄漏", err)
	}
	if _, err = f.service.ChatFileContent(ctx, f.a, f.second.ConversationID, file.ID); !errors.Is(err, chatlibrary.ErrNotFound) {
		t.Fatal("跨单聊文件泄漏", err)
	}
	lease := f.claim(t)
	f.start(t, lease)
	call := botagent.ToolCall{ID: ids.New(), Name: "chat_file_create", Arguments: memoryArguments(t, map[string]any{"name": "猫咪报告.md", "content": "# 已核对"})}
	encoded, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	operationRequest := OperationRequest{StepKey: "library.direct", Kind: "tool", ToolName: call.Name, RequestDigest: digest[:]}
	op, _, err := f.service.BeginOperation(ctx, lease, operationRequest)
	if err != nil {
		t.Fatal(err)
	}
	var tool chatFileTool
	for _, schema := range chatlibrary.Schemas() {
		if schema.Name == call.Name {
			tool = chatFileTool{executor: f.service, schema: schema, maximum: 8 << 10}
		}
	}
	invocation := Invocation{Lease: lease, OperationID: op.ID, Call: call}
	// 回执超限必须回滚文件、消息和收录事件，之后同一操作可正常执行。
	tooSmall := tool
	tooSmall.maximum = 1
	failure, err := tooSmall.Execute(ctx, invocation)
	if err != nil || !failure.IsError {
		t.Fatal("回执失败没有报告", failure, err)
	}
	before, err := f.service.ChatFiles(ctx, f.a, conversation, "", "猫咪")
	if err != nil || len(before.Files) != 0 {
		t.Fatal("失败工具留下半成品", before, err)
	}
	result, err := tool.Execute(ctx, invocation)
	if err != nil || result.IsError {
		t.Fatal(result, err)
	}
	saved, isCreated, err := f.service.BeginOperation(ctx, lease, operationRequest)
	if err != nil || isCreated || saved.Status != "succeeded" {
		t.Fatal("工具恢复不能重放回执", saved, err)
	}
	var generated chatlibrary.File
	if err = json.Unmarshal([]byte(result.Content), &generated); err != nil {
		t.Fatal(err)
	}
	page, err = f.service.ChatFiles(ctx, f.a, conversation, "", "")
	if err != nil || len(page.Files) != 2 {
		t.Fatal("未收录双方文件", page, err)
	}
	history, err := f.service.Messages(ctx, f.a, conversation, 0, 20)
	if err != nil || len(history) != 2 {
		t.Fatal("生成文件未直接发送或重复发送", history, err)
	}
	if history[0].Blocks[1].ChatFile.ID != generated.ID || !history[0].Blocks[0].UserMessage {
		t.Fatal("生成文件没有显示为猫咪消息")
	}
	reply := SendRequest{ClientRequestID: ids.New(), Content: "继续核对附件", ReplyToID: history[0].ID, FileIDs: []string{file.ID}}
	if _, err = f.service.Send(ctx, f.a, conversation, reply); err != nil {
		t.Fatal("工作中补充附件或回复生成文件失败", err)
	}
	history, err = f.service.Messages(ctx, f.a, conversation, 0, 20)
	if err != nil || history[0].Blocks[1].ChatFile.ID != file.ID {
		t.Fatal("补充消息丢失附件", history, err)
	}
	other, err := f.service.ChatFiles(ctx, f.a, f.second.ConversationID, "", "")
	if err != nil || len(other.Files) != 0 {
		t.Fatal("资料库混入其他猫咪文件", other, err)
	}
}
