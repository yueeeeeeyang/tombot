package usage_test

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sync"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/store"
	"io.yueyang/tom/internal/testkit"
	"io.yueyang/tom/internal/usage"
)

func chainFixture(t *testing.T) (*sql.DB, []domain.Run, domain.Model) {
	t.Helper()
	ctx := context.Background()
	db := testkit.Database(t)
	bots := service.NewBots(db, bus.New(db), t.TempDir())
	runs := []domain.Run{}
	for _, template := range []string{"researcher", "writer", "assistant"} {
		bot, err := bots.Create(ctx, service.BotInput{Template: template}, "user")
		if err != nil {
			t.Fatal(err)
		}
		run := domain.Run{ID: "run-" + template, TurnID: "turn-" + template, ConversationID: bot.ConversationID, BotID: bot.ID, ModelID: "test"}
		var parent *string
		if len(runs) > 0 {
			parent = &runs[len(runs)-1].ID
		}
		run.ParentRunID = parent
		if _, err = db.Exec(`INSERT INTO turns(id,conversation_id,seq,source,status,created_at) VALUES(?,?,1,'user','running',0)`, run.TurnID, run.ConversationID); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO runs(id,turn_id,conversation_id,bot_id,parent_run_id,source,status,model_id,prompt,budget_micro,created_at) VALUES(?,?,?,?,?,'bot_message','running','test','Test nested accounting.',100,0)`, run.ID, run.TurnID, run.ConversationID, run.BotID, parent); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
	}
	return db, runs, domain.Model{ID: "test", InputPriceMicro: 1000000, OutputPriceMicro: 1000000}
}

func TestNestedUsageAndBudgetAreSharedWithoutDuplicateEvents(t *testing.T) {
	db, runs, model := chainFixture(t)
	ctx := context.Background()
	for index, tokens := range []int{7, 11} {
		if _, err := usage.Record(ctx, db, runs[index+1], model, provider.Usage{InputTokens: tokens}); err != nil {
			t.Fatal(err)
		}
	}
	for index, expected := range []int64{18, 18, 11} {
		var cost, turnCost int64
		if err := db.QueryRow("SELECT cost_micro FROM runs WHERE id=?", runs[index].ID).Scan(&cost); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow("SELECT cost_micro FROM turns WHERE id=?", runs[index].TurnID).Scan(&turnCost); err != nil {
			t.Fatal(err)
		}
		if cost != expected || turnCost != expected {
			t.Fatal("祖先费用累计错误", index, cost, turnCost)
		}
	}
	var remaining int64
	if err := store.Transact(ctx, db, func(tx *sql.Tx) error {
		var err error
		remaining, err = usage.RemainingBudget(ctx, tx, runs[2].ID, 1000)
		return err
	}); err != nil || remaining != 82 {
		t.Fatal("预算未取最小余额", remaining, err)
	}
	var count, cost int64
	if err := db.QueryRow("SELECT COUNT(*),SUM(cost_micro) FROM usage_events").Scan(&count, &cost); err != nil || count != 2 || cost != 18 {
		t.Fatal("实际费用事件被重复记账", count, cost, err)
	}
	if _, err := db.Exec("UPDATE runs SET budget_micro=20 WHERE id=?", runs[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := usage.Record(ctx, db, runs[2], model, provider.Usage{InputTokens: 3}); err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		var public *domain.Error
		if err := usage.CheckBudget(ctx, db, run.ID); !errors.As(err, &public) || public.Code != "RUN_BUDGET_EXCEEDED" {
			t.Fatal("后代绕过祖先预算", run.ID, err)
		}
	}
}

func TestConcurrentDescendantCostAndFailedAncestryRollback(t *testing.T) {
	db, runs, model := chainFixture(t)
	ctx := context.Background()
	failures := make(chan error, 20)
	var wg sync.WaitGroup
	for index := range 20 {
		wg.Go(func() {
			_, err := usage.Record(ctx, db, runs[1+index%2], model, provider.Usage{InputTokens: 1})
			failures <- err
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var cost int64
	if err := db.QueryRow("SELECT cost_micro FROM runs WHERE id=?", runs[0].ID).Scan(&cost); err != nil || cost != 20 {
		t.Fatal("并发结算丢失", cost, err)
	}
	if _, err := db.Exec("UPDATE runs SET cost_micro=? WHERE id=?", int64(math.MaxInt64), runs[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := usage.Record(ctx, db, runs[2], model, provider.Usage{InputTokens: 1}); err == nil {
		t.Fatal("祖先溢出未阻止结算")
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM usage_events").Scan(&count); err != nil || count != 20 {
		t.Fatal("失败结算未整体回滚", count, err)
	}
	if _, err := db.Exec("UPDATE runs SET parent_run_id=? WHERE id=?", runs[2].ID, runs[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := usage.CheckBudget(ctx, db, runs[2].ID); err == nil {
		t.Fatal("预算父链循环被接受")
	}
	if _, err := usage.Record(ctx, db, runs[2], model, provider.Usage{InputTokens: 1}); err == nil {
		t.Fatal("循环父链仍被结算")
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM usage_events").Scan(&count); err != nil || count != 20 {
		t.Fatal("父链错误遗留费用", count, err)
	}
}
