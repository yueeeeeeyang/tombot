package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provider"
)

func TestSharedAttachmentsReachGroupMembersWithoutPrivateToolResults(t *testing.T) {
	received := make(chan provider.Request, 10)
	var coordinator string
	r, bot := runtimeFixture(t, script(func(ctx context.Context, in provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		received <- in
		if strings.HasPrefix(in.System, "You are the router") {
			return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: fmt.Sprintf(`{"steps":[{"bot_id":%q,"brief":"Review the attachment."}],"reason":"Relevant role."}`, coordinator)})
		}
		return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "已处理共享附件"})
	}), DefaultLimits())
	ctx := t.Context()
	coordinator = bot.ID
	group := createTestGroup(t, r, bot, "coordinator", false, 5_000_000)
	file, err := r.deps.Files.UploadShared(ctx, "共享.txt", strings.NewReader("公开材料"), func(context.Context, string, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.AttachmentInput{Path: file.Path, SHA256: file.SHA256}
	rejection := func(conversation string, refs []domain.AttachmentInput, code string) {
		t.Helper()
		_, err := r.Submit(ctx, conversation, SendInput{Attachments: refs}, "user")
		var public *domain.Error
		if !errors.As(err, &public) || public.Code != code {
			t.Fatalf("应返回 %s，实际 %v", code, err)
		}
	}
	rejection(bot.ConversationID, []domain.AttachmentInput{ref}, "ATTACHMENT_SCOPE")
	rejection(group.ConversationID, []domain.AttachmentInput{{Path: strings.TrimPrefix(ref.Path, "shared/"), SHA256: ref.SHA256}}, "ATTACHMENT_SCOPE")
	rejection(group.ConversationID, []domain.AttachmentInput{ref, ref}, "ATTACHMENT_SCOPE")
	rejection(group.ConversationID, make([]domain.AttachmentInput, 5), "ATTACHMENT_LIMIT")
	rejection(group.ConversationID, []domain.AttachmentInput{{Path: ref.Path, SHA256: strings.Repeat("0", 64)}}, "ATTACHMENT_CHANGED")
	sent, err := r.Submit(ctx, group.ConversationID, SendInput{Attachments: []domain.AttachmentInput{ref}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	status, code, _ := waitGroupTurn(t, r, sent.TurnID)
	if status != "succeeded" {
		t.Fatal(status, code)
	}
	for _, stage := range []string{"规划者", "执行成员"} {
		select {
		case req := <-received:
			if ref := firstAttachment(req); ref == nil || ref.Path != file.Path || string(ref.Content) != "公开材料" {
				t.Fatal(stage, "未收到共享材料")
			}
		default:
			t.Fatal(stage, "缺少模型请求")
		}
	}
	private := domain.ArtifactRef{AttachmentInput: domain.AttachmentInput{Path: "inbox/private/screen.jpg"}}
	if peer := collaborationTranscript("system", "other", []domain.MessageBlock{{Kind: "tool_result", ToolResult: &domain.ToolResultBlock{Artifact: &private, Content: "private"}}}); len(peer) != 0 {
		t.Fatal("私有截图进入同事转录")
	}
}

func firstAttachment(req provider.Request) *domain.ArtifactRef {
	for _, message := range req.Messages {
		for _, block := range message.Blocks {
			if block.Kind == "artifact" {
				return block.Artifact
			}
		}
	}
	return nil
}

func TestRetainedAttachmentFollowupAndReplacement(t *testing.T) {
	received := make(chan provider.Request, 3)
	r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		received <- req
		return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "已读"})
	}), DefaultLimits())
	before := func(context.Context, string, string) error { return nil }
	file, err := r.deps.Files.Upload(t.Context(), bot.ID, "材料.txt", strings.NewReader("原始内容"), before)
	if err != nil {
		t.Fatal(err)
	}
	send := func(input SendInput) *domain.ArtifactRef {
		t.Helper()
		sent, err := r.Submit(t.Context(), bot.ConversationID, input, "user")
		if err != nil {
			t.Fatal(err)
		}
		if run := waitRun(t, r, sent.RunID); run.Status != "succeeded" {
			t.Fatal(run.Status, run.ErrorCode)
		}
		ref := firstAttachment(<-received)
		if ref == nil || ref.SHA256 != file.SHA256 {
			t.Fatal("模型未收到原始附件的标识")
		}
		return ref
	}
	ref := send(SendInput{Attachments: []domain.AttachmentInput{{Path: file.Path, SHA256: file.SHA256}}})
	if string(ref.Content) != "原始内容" {
		t.Fatal("当前回合没有明文")
	}
	ref = send(SendInput{Content: "继续分析刚才的材料"})
	if string(ref.Content) != "原始内容" {
		t.Fatal("后续回合没有取回同一份材料")
	}
	if _, err = r.deps.Files.UpdateText(t.Context(), bot.ID, file.Path, "后写内容", file.SHA256, before); err != nil {
		t.Fatal(err)
	}
	ref = send(SendInput{Content: "再检查一次"})
	if !ref.Unavailable || len(ref.Content) != 0 {
		t.Fatal("不应把替换后的内容当作原始材料")
	}
}
