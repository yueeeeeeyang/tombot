package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/chatlibrary"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computertools"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

type computerFilesFunc func(context.Context, computer.FileRequest) (computer.FileReceipt, error)

func (call computerFilesFunc) TransferFile(ctx context.Context, request computer.FileRequest) (computer.FileReceipt, error) {
	return call(ctx, request)
}

// TestComputerLibraryAgentLoop 保留真实资料库、租约、操作账本和 Agent 协议，只替代节点文件字节传输。
func TestComputerLibraryAgentLoop(t *testing.T) {
	cluster := pgtest.Start(t)
	for _, mode := range []string{"单聊", "群聊", "损坏回包", "跨会话文件", "导入中断"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, cluster)
			f.service.options.MaxOperations = 16
			f.visionModel(t)
			spec := f.first.Spec
			spec.ToolPermissions = []string{"computer_shell"}
			bot, err := f.catalog.Update(t.Context(), f.a, f.first.ID, bots.UpdateRequest{ExpectedRevision: f.first.Revision, Spec: spec})
			if err != nil {
				t.Fatal(err)
			}
			conversation := bot.ConversationID
			mentions := []string(nil)
			if mode == "群聊" {
				group := f.group(t)
				conversation, mentions = group.ConversationID, []string{bot.ID}
			}
			content := bytes.Repeat([]byte{0xff, 0, 1, 2, 13, 10}, 10000)
			digest := sha256.Sum256(content)
			file, err := f.service.UploadChatFile(t.Context(), f.a, conversation, chatlibrary.Upload{ClientRequestID: ids.New(), Name: "输入.bin", Content: content})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.service.Send(t.Context(), f.a, conversation, SendRequest{ClientRequestID: ids.New(), Content: "读取附件并交付结果", Mentions: mentions, FileIDs: []string{file.ID}}); err != nil {
				t.Fatal(err)
			}
			importID := file.ID
			if mode == "跨会话文件" {
				foreign, failure := f.service.UploadChatFile(t.Context(), f.a, f.second.ConversationID, chatlibrary.Upload{ClientRequestID: ids.New(), Name: "其他.bin", Content: content})
				if failure != nil {
					t.Fatal(failure)
				}
				importID = foreign.ID
			}
			lease := f.claim(t)
			models, calls, commits, closes := 0, 0, 0, 0
			var imported []byte
			receipts := map[string]computer.FileReceipt{}
			files := computerFilesFunc(func(_ context.Context, request computer.FileRequest) (computer.FileReceipt, error) {
				calls++
				if request.Validate() != nil || request.Session.BotID != bot.ID || request.Session.UserID != f.a.UserID || request.Session.RunID != lease.RunID {
					t.Fatal("节点收到其他电脑或无效输入")
				}
				var count int
				if err := f.pool.QueryRow(`SELECT count(*) FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2 AND id=$3 AND computer_session_id=$4 AND kind='computer' AND status IN ('started','succeeded')`, lease.UserID, lease.RunID, request.OperationID, request.Session.SessionID).Scan(&count); err != nil || count != 1 {
					t.Fatal("文件动作未先记账", err)
				}
				receipt := receipts[request.OperationID]
				switch request.Action {
				case "import_start":
					if !strings.HasPrefix(request.Path, "/workspace/Library/") || request.Size != len(content) || request.SHA256 != file.SHA256 {
						t.Fatal("导入参数错误")
					}
					receipt = computer.FileReceipt{Session: request.Session, OperationID: request.OperationID, State: "receiving", Path: request.Path, Size: request.Size, SHA256: request.SHA256}
				case "import_chunk":
					if mode == "导入中断" {
						return computer.FileReceipt{}, errors.New("节点传输中断")
					}
					if request.Offset != len(imported) {
						t.Fatal("分块错序")
					}
					imported = append(imported, request.Bytes...)
					receipt.Offset = len(imported)
				case "import_commit":
					commits++
					receipt.State = "saved"
				case "export_start":
					if request.Path != "/workspace/Downloads/结果.bin" {
						t.Fatal("导出路径错误")
					}
					receipt = computer.FileReceipt{Session: request.Session, OperationID: request.OperationID, State: "ready", Path: request.Path, Size: len(content), SHA256: hex.EncodeToString(digest[:])}
				case "export_chunk":
					receipt.Offset = request.Offset
					receipt.Bytes = bytes.Clone(content[request.Offset:min(request.Offset+computer.MaximumFileChunkBytes, len(content))])
					if mode == "损坏回包" {
						receipt.Bytes[0] ^= 1
					}
				case "close":
					closes++
					receipt.State, receipt.Bytes = "closed", nil
				default:
					t.Fatal("未预期动作", request.Action)
				}
				receipts[request.OperationID] = receipt
				return receipt, nil
			})
			node := &shellNode{sessionNode: &sessionNode{}}
			shell, err := NewComputerShell(node, computertools.ShellOptions{CallTimeout: time.Second, PollInterval: 10 * time.Millisecond, MaximumResultBytes: 8 << 10})
			if err != nil {
				t.Fatal(err)
			}
			worker := f.workerWithComputers(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
				models++
				available := map[string]bool{}
				for _, tool := range input.Tools {
					available[tool.Name] = true
				}
				if !available["chat_file_import"] || !available["chat_file_export"] {
					t.Fatal("文件工具未注册")
				}
				switch models {
				case 1:
					return emitDesktopTool(emit, models, "chat_file_import", memoryArguments(t, map[string]string{"id": importID}))
				case 2:
					result := input.Messages[len(input.Messages)-1].Blocks[0].ToolResult
					if result == nil || result.IsError || !strings.Contains(result.Content, "/workspace/Library/") {
						t.Fatal("导入路径未返回")
					}
					return emitDesktopTool(emit, models, "chat_file_export", `{"path":"/workspace/Downloads/结果.bin","name":"交付.bin"}`)
				default:
					result := input.Messages[len(input.Messages)-1].Blocks[0].ToolResult
					var exported chatlibrary.File
					if result == nil || json.Unmarshal([]byte(result.Content), &exported) != nil || exported.Name != "交付.bin" {
						t.Fatal("附件未返回模型")
					}
					return emitText(emit, "已交付文件。")
				}
			}), f.computers(t, node, computerOptions()), shell)
			worker.agents.(*workerAgents).files = files
			worker.files = files
			worker.options.MaxModelSteps = 4
			run, err := worker.Process(t.Context(), lease)
			if err != nil {
				t.Fatal(err)
			}
			page, err := f.service.ChatFiles(t.Context(), f.a, conversation, "", "交付")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "单聊" || mode == "群聊" {
				if run.Status != "succeeded" || len(page.Files) != 1 || commits != 1 || closes != 2 || !bytes.Equal(imported, content) {
					t.Fatal("文件闭环未完成", run.Status, run.ErrorCode, len(page.Files), commits, closes, models, calls)
				}
				output, err := f.service.ChatFileContent(t.Context(), f.a, conversation, page.Files[0].ID)
				if err != nil || !bytes.Equal(output.Bytes, content) {
					t.Fatal("交付字节不一致", err)
				}
			} else {
				if run.Status == "succeeded" || len(page.Files) != 0 {
					t.Fatal("失败仍发布附件", run.Status)
				}
				if mode == "跨会话文件" && calls != 0 {
					t.Fatal("越权文件传至节点")
				}
				if mode == "导入中断" && (commits != 0 || closes != 1) {
					t.Fatal("中断后仍提交或未关闭", run.ErrorCode, models, calls)
				}
			}
			if f.sessionState(t, lease) != "released" {
				t.Fatal("未释放桌面")
			}
		})
	}
}
