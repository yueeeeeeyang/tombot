package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/agent"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
)

type sessionBooking struct {
	session, start, room string
}

func (b sessionBooking) confirmedIn(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, strings.ToLower(b.session)) && strings.Contains(text, strings.TrimLeft(b.start, "0")) && strings.Contains(text, strings.ToLower(b.room))
}

// TestLocalModelGroupAssignment 将三条可公开的预订凭据分别交给成员，群消息本身不提供时间或场地。
func TestLocalModelGroupAssignment(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	server, device := localModelFixture(t, ctx, 384)
	var members []string
	var coordinator domain.Bot
	bookings := map[string]sessionBooking{}
	originals := map[string]domain.Memory{}
	for _, booking := range []sessionBooking{{"Registration", "09:15", "Cedar"}, {"Workshop", "10:30", "Maple"}, {"Lunch", "12:45", "Birch"}} {
		bot, err := server.Bots.Create(ctx, service.BotInput{
			Name: booking.session, RoleTitle: "Event scheduling assistant",
			Soul: "You maintain a session booking. When asked for the public schedule, quote the confirmed sentence from your booking record, including the exact session, time and room.",
		}, "user")
		if err != nil {
			t.Fatal(err)
		}
		if len(members) == 0 {
			coordinator = bot
		}
		fact := booking.session + " starts at " + booking.start + " in the " + booking.room + " room."
		original, err := server.Memories.Write(ctx, bot.ID, "", domain.MemoryInput{
			Title: "Confirmed public booking", Content: fact, Summary: fact, Importance: 3,
		}, "", "user")
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, bot.ID)
		bookings[bot.ID], originals[bot.ID] = booking, original
	}
	const task = "What is our confirmed event schedule? Each member should quote their own booking record. In the final agenda, list each confirmed session with its time and room on its own line."
	group, err := server.Groups.Create(ctx, service.GroupInput{Name: "活动日程核对", Purpose: task, Members: members}, "user")
	if err != nil {
		t.Fatal(err)
	}
	var sent agent.Submission
	if os.Getenv("TOM_TEST_NATIVE_GROUP") == "1" {
		runNativeBotScenario(t, server, coordinator, device, "group_assignment_test.dart")
	} else {
		response := request(t, server.Handler(), http.MethodPost, "/api/v1/conversations/"+group.ConversationID+"/messages", agent.SendInput{Content: task}, map[string]string{"Authorization": "Bearer " + device})
		if response.Code != http.StatusAccepted {
			t.Fatal("日程任务未受理", response.Code)
		}
		if err = json.Unmarshal(response.Body.Bytes(), &sent); err != nil || sent.TurnID == "" {
			t.Fatal("缺少群回合标识", err)
		}
	}
	var snapshot service.ConversationSnapshot
	for {
		wake := server.Bus.Wake()
		snapshot, err = server.Conversations.Snapshot(ctx, group.ConversationID)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.ActiveTurnID == nil {
			break
		}
		select {
		case <-wake:
		case <-ctx.Done():
			t.Fatal("日程核对超时", ctx.Err())
		}
	}
	turns, err := server.Runtime.Turns(ctx, group.ConversationID, "", 2)
	if err != nil || len(turns) != 1 || turns[0].Status != "succeeded" || (sent.TurnID != "" && turns[0].ID != sent.TurnID) {
		t.Fatal("群任务没有正常完成", turns, err)
	}
	confirmed := map[string]bool{}
	for _, message := range snapshot.Messages {
		if message.SenderKind != "bot" {
			continue
		}
		if message.RunID == nil || message.SenderBotID == nil || message.TokenIn <= 0 || message.TokenOut <= 0 || message.Status != "complete" {
			t.Fatal("成员回复缺少身份、真实用量或完成状态", message.Seq)
		}
		run, err := server.Runtime.Run(ctx, *message.RunID)
		if err != nil || run.Status != "succeeded" || run.BotID != *message.SenderBotID || run.TurnID != turns[0].ID {
			t.Fatal("回复未关联成功的本回合执行", run, err)
		}
		var body strings.Builder
		for _, block := range message.Blocks {
			if block.Kind == "text" {
				body.WriteString(block.Text)
			}
		}
		if run.PlanRole != "member" {
			t.Fatal("出现非平等成员身份", run.PlanRole)
		}
		booking, ok := bookings[run.BotID]
		if !ok || confirmed[run.BotID] || !booking.confirmedIn(body.String()) {
			t.Fatalf("成员预订核对失败 %s：%s", booking.session, body.String())
		}
		confirmed[run.BotID] = true
	}
	if len(confirmed) != len(members) {
		t.Fatal("缺少成员原话", len(confirmed))
	}
	for _, member := range members {
		memories, err := server.Memories.List(ctx, member)
		if err != nil || len(memories) != 1 || memories[0].Path != originals[member].Path || memories[0].SHA256 != originals[member].SHA256 {
			t.Fatal("群任务后成员私有记忆发生变化", bookings[member].session, err)
		}
	}
	t.Log("三名成员各自核对了预订，保留了三组对应的时间地点原话，三份私有记忆校验一致")
}
