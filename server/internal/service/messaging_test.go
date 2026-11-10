package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/testkit"
)

func TestBotPrivateConversationIdentityAndDeletionGuard(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	events := bus.New(db)
	bots := NewBots(db, events, t.TempDir())
	conversations := NewConversations(db, events)
	sender, err := bots.Create(ctx, BotInput{Template: "researcher"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	target, err := bots.Create(ctx, BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO conversations(id,kind,source,bot_id,initiator_bot_id,title,created_at,updated_at) VALUES('private','bot_dm','bot_message',?,?,'双方私信',0,0)`, target.ID, sender.ID); err != nil {
		t.Fatal(err)
	}
	target, err = bots.Update(ctx, target.ID, BotPatch{Name: Field[string]{Set: true, Value: "编辑伙伴"}, AccentHue: Field[int]{Set: true, Value: 240}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := conversations.Get(ctx, "private")
	if err != nil || conversation.Title != sender.Name+" ↔ 编辑伙伴" || len(conversation.Members) != 2 {
		t.Fatal(conversation, err)
	}
	found := false
	for _, member := range conversation.Members {
		if member.BotID == target.ID {
			found = member.Name == target.Name && member.AccentHue == 240
		}
	}
	encoded, err := json.Marshal(conversation.Members)
	if err != nil || strings.Contains(string(encoded), target.Soul) || !found {
		t.Fatal("私信成员身份错误", string(encoded), err)
	}
	if _, err = db.Exec(`INSERT INTO turns(id,conversation_id,seq,source,status,created_at) VALUES('private-turn','private',1,'user','running',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO runs(id,turn_id,conversation_id,bot_id,source,status,model_id,prompt,created_at) VALUES('private-run','private-turn','private',?,'chat','running','fixture','User intervention.',0)`, target.ID); err != nil {
		t.Fatal(err)
	}
	var public *domain.Error
	if err = bots.Delete(ctx, sender.ID, sender.Name, "user", false); !errors.As(err, &public) || public.Code != "BOT_BUSY" {
		t.Fatal("对方仍在私信执行时允许删除", err)
	}
	if _, err = db.Exec(`UPDATE runs SET status='succeeded' WHERE id='private-run'`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE turns SET status='succeeded' WHERE id='private-turn'`); err != nil {
		t.Fatal(err)
	}
	if err = bots.Delete(ctx, sender.ID, sender.Name, "user", false); err != nil {
		t.Fatal(err)
	}
	if _, err = conversations.Get(ctx, "private"); err == nil {
		t.Fatal("删除后仍有私信残留")
	}
}

func TestBotMessageContextBoundaries(t *testing.T) {
	valid := domain.BotMessageRequest{BotID: "target", Message: strings.Repeat("m", domain.MaxMessageBytes), ContextFiles: []domain.ContextFile{{Path: "shared/brief.md", Content: strings.Repeat("c", domain.MaxMessageContextBytes)}}}
	if err := ValidateBotMessage(valid); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*domain.BotMessageRequest){
		func(input *domain.BotMessageRequest) { input.Message += "x" },
		func(input *domain.BotMessageRequest) { input.Message = " " },
		func(input *domain.BotMessageRequest) {
			input.ContextFiles = append(input.ContextFiles, domain.ContextFile{Path: "shared/second.md", Content: "x"})
		},
		func(input *domain.BotMessageRequest) {
			input.ContextFiles = append(input.ContextFiles, input.ContextFiles[0])
		},
		func(input *domain.BotMessageRequest) {
			input.ContextFiles = make([]domain.ContextFile, domain.MaxMessageContextFiles+1)
		},
	} {
		invalid := valid
		change(&invalid)
		if err := ValidateBotMessage(invalid); err == nil {
			t.Fatal("越界上下文被接受")
		}
	}
}
