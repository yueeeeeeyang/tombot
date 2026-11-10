package service

import (
	"context"
	"database/sql"
	"fmt"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/store"
	"testing"
	"time"
)

func TestGroupArtifactsPagingAndChatIsolation(t *testing.T) {
	groups, _, members := groupFixture(t)
	ctx := context.Background()
	group, err := groups.Create(ctx, GroupInput{Name: "资料群", Members: []string{members[0].ID}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, groups.db, func(tx *sql.Tx) error {
		for index := 0; index < 52; index++ {
			message := domain.Message{ID: fmt.Sprint("file-", index), ConversationID: group.ConversationID, SenderKind: "user", Status: "complete", CreatedAt: time.Now(), Blocks: []domain.MessageBlock{{Kind: "artifact", Artifact: &domain.ArtifactRef{AttachmentInput: domain.AttachmentInput{Path: fmt.Sprintf("shared/inbox/report-%d.pdf", index)}, MediaType: "application/pdf"}}}}
			if err := AppendMessage(ctx, tx, &message); err != nil {
				return err
			}
		}
		private := domain.Message{ID: "private", ConversationID: members[0].ConversationID, SenderKind: "user", Status: "complete", CreatedAt: time.Now(), Blocks: []domain.MessageBlock{{Kind: "artifact", Artifact: &domain.ArtifactRef{AttachmentInput: domain.AttachmentInput{Path: "shared/inbox/private.pdf"}}}}}
		return AppendMessage(ctx, tx, &private)
	}); err != nil {
		t.Fatal(err)
	}
	first, err := groups.Artifacts(ctx, group.ID, 0)
	if err != nil || len(first.Artifacts) != 50 || first.Next == 0 {
		t.Fatal(first, err)
	}
	second, err := groups.Artifacts(ctx, group.ID, first.Next)
	if err != nil || len(second.Artifacts) != 2 || second.Next != 0 {
		t.Fatal(second, err)
	}
	seen := map[string]bool{}
	for _, artifact := range append(first.Artifacts, second.Artifacts...) {
		if seen[artifact.ID] || artifact.Title == "private.pdf" {
			t.Fatal("分页重复或私聊资料泄露", artifact)
		}
		seen[artifact.ID] = true
	}
	if _, err := groups.Artifacts(ctx, group.ID, -1); err == nil {
		t.Fatal("接受非法游标")
	}
	if _, err := groups.Artifacts(ctx, "missing", 0); err == nil {
		t.Fatal("不存在的群被当成空资料库")
	}
}
