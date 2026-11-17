package execution

import (
	"context"
	"errors"
	"sync"
	"testing"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func (f fixture) multiPolicy(t *testing.T, mode string) modelregistry.Version {
	t.Helper()
	ctx := context.Background()
	var providerID string
	if err := f.pool.QueryRow(`SELECT provider_id FROM tombot.models WHERE id=$1`, f.modelID).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	model, err := f.registry.CreateModel(ctx, f.actor, ids.New(), modelregistry.ModelRequest{ProviderID: providerID, ModelKey: "routing-backup", DisplayName: "备用视觉模型", ContextLimit: 16384, OutputLimit: 512, SupportsTools: true, SupportsVision: true})
	if err != nil {
		t.Fatal(err)
	}
	v, err := f.registry.CreateVersion(ctx, f.actor, ids.New(), modelregistry.VersionRequest{PolicyID: f.policyID, RoutingMode: mode, Candidates: []modelregistry.CandidateRequest{
		{ModelID: f.modelID, Priority: 1, Weight: 8, OutputLimit: routingInt(256)}, {ModelID: model.ID, Priority: 2, Weight: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	v, err = f.registry.EnableVersion(ctx, f.actor, ids.New(), f.policyID, v.Number, f.revision(t))
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func (f fixture) selection(t *testing.T, bot bots.Bot) (Lease, modelregistry.Selection) {
	t.Helper()
	f.send(t, f.a, bot)
	lease := f.claim(t)
	f.start(t, lease)
	selected, err := f.service.BindModel(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	return lease, selected
}

func TestModelRoutingIntegration(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("能力筛选与跨用户隔离", func(t *testing.T) {
		f := newFixture(t, cluster)
		v := f.multiPolicy(t, "priority")
		_, selection := f.selection(t, f.first)
		selected, err := f.registry.Route(ctx, selection, modelregistry.Requirements{Tools: true, Vision: true, InputTokens: 100}, "")
		if err != nil || selected.ModelID != v.Candidates[1].ModelID {
			t.Fatal("未按能力筛选", err)
		}
		repeated, err := f.registry.Route(ctx, selection, modelregistry.Requirements{}, "")
		if err != nil || repeated.ModelID != selected.ModelID {
			t.Fatal("重复选择改变模型", err)
		}
		_, err = f.registry.Route(ctx, selection, modelregistry.Requirements{InputTokens: max(v.Candidates[0].Configuration.ContextLimit, v.Candidates[1].Configuration.ContextLimit) + 1}, "")
		expect(t, err, modelregistry.ErrNoCandidate)
		forged := selection
		forged.UserID = f.b.UserID
		forged.BotID = f.foreign.ID
		if _, err = f.registry.Route(ctx, forged, modelregistry.Requirements{}, ""); err == nil {
			t.Fatal("跨用户路由成功")
		}
	})
	t.Run("主备选择不重复且停用候选不参与", func(t *testing.T) {
		f := newFixture(t, cluster)
		v := f.multiPolicy(t, "priority")
		_, selection := f.selection(t, f.first)
		first, err := f.registry.Route(ctx, selection, modelregistry.Requirements{}, "")
		if err != nil || first.ModelID != f.modelID {
			t.Fatal("优先级选择错误", err)
		}
		next, err := f.registry.Route(ctx, first, modelregistry.Requirements{}, first.ModelID)
		if err != nil || next.ModelID != v.Candidates[1].ModelID {
			t.Fatal("未切换备用", err)
		}
		same, err := f.registry.Route(ctx, first, modelregistry.Requirements{}, first.ModelID)
		if err != nil || same.ModelID != next.ModelID {
			t.Fatal("重复故障推进路由", err)
		}
		_, err = f.registry.Route(ctx, next, modelregistry.Requirements{}, next.ModelID)
		expect(t, err, modelregistry.ErrNoCandidate)
		if err = f.registry.SetEnabled(ctx, f.actor, ids.New(), "model", f.modelID, 1, false); err != nil {
			t.Fatal(err)
		}
		_, other := f.selection(t, f.second)
		selected, err := f.registry.Route(ctx, other, modelregistry.Requirements{}, "")
		if err != nil || selected.ModelID != next.ModelID {
			t.Fatal("停用候选被选中", err)
		}
	})
	t.Run("轮询跨运行并发及重复请求一致", func(t *testing.T) {
		f := newFixture(t, cluster)
		v := f.multiPolicy(t, "round_robin")
		selections := []modelregistry.Selection{}
		for range 6 {
			spec := f.first.Spec
			spec.Name = ids.New()
			bot, _, err := f.catalog.Create(ctx, f.a, bots.CreateRequest{ClientRequestID: ids.New(), Spec: spec})
			if err != nil {
				t.Fatal(err)
			}
			_, selected := f.selection(t, bot)
			selections = append(selections, selected)
		}
		var wg sync.WaitGroup
		results := make(chan modelregistry.Selection, len(selections)*2)
		failures := make(chan error, len(selections)*2)
		for _, selected := range selections {
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					result, err := f.registry.Route(ctx, selected, modelregistry.Requirements{}, "")
					if err != nil {
						failures <- err
					} else {
						results <- result
					}
				}()
			}
		}
		wg.Wait()
		close(results)
		close(failures)
		for err := range failures {
			t.Error(err)
		}
		byRun := map[string]string{}
		counts := map[string]int{}
		for selected := range results {
			if old, ok := byRun[selected.RunID]; ok {
				if old != selected.ModelID {
					t.Error("同运行分配不同模型")
				}
			} else {
				byRun[selected.RunID] = selected.ModelID
				counts[selected.ModelID]++
			}
		}
		if counts[f.modelID] != 3 || counts[v.Candidates[1].ModelID] != 3 {
			t.Fatal("轮询不均匀", counts)
		}
		var ticket int
		if err := f.pool.QueryRow(`SELECT next_ticket FROM tombot.model_route_counters WHERE policy_id=$1 AND version=$2`, f.policyID, v.Number).Scan(&ticket); err != nil || ticket != 6 {
			t.Fatal("重复请求消耗轮询位置", ticket, err)
		}
	})
	t.Run("加权路由持久保存且不会按调用重新抽取", func(t *testing.T) {
		f := newFixture(t, cluster)
		v := f.multiPolicy(t, "weighted")
		_, selection := f.selection(t, f.first)
		selected, err := f.registry.Route(ctx, selection, modelregistry.Requirements{}, "")
		if err != nil {
			t.Fatal(err)
		}
		if selected.ModelID != v.Candidates[0].ModelID && selected.ModelID != v.Candidates[1].ModelID {
			t.Fatal("路由候选越界")
		}
		for range 8 {
			next, err := f.registry.Route(ctx, selection, modelregistry.Requirements{}, "")
			if err != nil || next.ModelID != selected.ModelID {
				t.Fatal("加权重复抽取", err)
			}
		}
		if _, err = f.registry.EnableVersion(ctx, f.actor, ids.New(), f.policyID, 1, f.revision(t)); err != nil {
			t.Fatal(err)
		}
		still, err := f.registry.Route(ctx, selection, modelregistry.Requirements{}, "")
		if err != nil || still.Version != v.Number || still.ModelID != selected.ModelID {
			t.Fatal("切换版本覆盖在途路由", err)
		}
		_, newSelection := f.selection(t, f.second)
		if newSelection.Version != 1 {
			t.Fatal("旧版本未启用")
		}
		if err = f.registry.DisableVersion(ctx, f.actor, ids.New(), f.policyID, 1, f.revision(t)); err != nil {
			t.Fatal(err)
		}
		_, err = f.registry.Resolve(ctx, f.a.UserID, f.first.ID)
		expect(t, err, modelregistry.ErrUnavailable)
		still, err = f.registry.Route(ctx, selection, modelregistry.Requirements{}, "")
		if err != nil || still.Version != v.Number || still.ModelID != selected.ModelID {
			t.Fatal("停用版本覆盖在途路由", err)
		}
	})
}

func TestWorkerModelFailover(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	for _, test := range []struct {
		name           string
		partial, retry bool
		want           string
		calls          int
	}{
		{"无输出暂时性失败切换并保留未知用量", false, true, "succeeded", 2},
		{"部分输出失败不切换", true, true, "unknown", 1},
		{"认证等不可重试失败不切换", false, false, "unknown", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, cluster)
			f.multiPolicy(t, "priority")
			bot := f.plainBot(t)
			calls := 0
			worker := f.worker(t, callerFunc(func(_ context.Context, c modelregistry.Configuration, _ []byte, _ modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
				calls++
				if c.ModelKey == "execution-fixture" {
					if test.partial {
						if err := emit(provider.Event{Kind: "text_delta", Text: "部分内容"}); err != nil {
							return modelgateway.Result{}, err
						}
					}
					return modelgateway.Result{CanFailover: test.retry}, errors.New("暂时性测试失败")
				}
				return emitText(emit, "备用模型完成")
			}))
			f.send(t, f.a, bot)
			lease := f.claim(t)
			run, err := worker.Process(ctx, lease)
			if err != nil || run.Status != test.want || calls != test.calls {
				t.Fatal("故障切换结果错误", run.Status, calls, err)
			}
			if test.want == "succeeded" {
				var unknown, known int
				if err = f.pool.QueryRow(`SELECT count(*) FILTER(WHERE state='unknown'),count(*) FILTER(WHERE state='settled') FROM tombot.usage_reservations WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID).Scan(&unknown, &known); err != nil || unknown != 1 || known != 1 {
					t.Fatal("失败调用用量被伪造", unknown, known, err)
				}
				var models int
				if err = f.pool.QueryRow(`SELECT count(DISTINCT model_id) FROM tombot.usage_reservations WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID).Scan(&models); err != nil || models != 2 {
					t.Fatal("计量未固定各次候选", err)
				}
			}
		})
	}
}

func TestFailoverRecoveryDoesNotReplayTools(t *testing.T) {
	cluster := pgtest.Start(t)
	f := newFixture(t, cluster)
	ctx := context.Background()
	var providerID string
	if err := f.pool.QueryRow(`SELECT provider_id FROM tombot.models WHERE id=$1`, f.modelID).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	if err := f.registry.SetEnabled(ctx, f.actor, ids.New(), "model", f.modelID, 1, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.registry.UpdateModel(ctx, f.actor, ids.New(), f.modelID, 2, modelregistry.ModelRequest{ModelKey: "execution-fixture", DisplayName: "工具主模型", ContextLimit: 16384, OutputLimit: 1024, SupportsTools: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.registry.SetEnabled(ctx, f.actor, ids.New(), "model", f.modelID, 3, true); err != nil {
		t.Fatal(err)
	}
	f.multiPolicy(t, "priority")
	calls, tools := 0, 0
	worker := f.worker(t, callerFunc(func(_ context.Context, c modelregistry.Configuration, _ []byte, _ modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
		calls++
		if calls == 1 {
			return emitTool(emit, `{"path":"report.txt"}`)
		}
		if c.ModelKey == "execution-fixture" {
			return modelgateway.Result{CanFailover: true}, errors.New("暂时性失败")
		}
		return emitText(emit, "工具结果已核对")
	}), testTool{execute: func(context.Context, Invocation) (botagent.ToolResult, error) {
		tools++
		return botagent.ToolResult{Content: "Verified contents."}, nil
	}})
	f.send(t, f.a, f.first)
	lease := f.claim(t)
	execute(t, f.pool, `CREATE FUNCTION tombot.reject_routing_finish() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN IF NEW.kind='run.finished' THEN RAISE EXCEPTION '最终记录失败'; END IF; RETURN NEW; END $body$`)
	execute(t, f.pool, `CREATE TRIGGER reject_routing_finish BEFORE INSERT ON tombot.user_events FOR EACH ROW EXECUTE FUNCTION tombot.reject_routing_finish()`)
	if _, err := worker.Process(ctx, lease); err == nil {
		t.Fatal("最终记录失败被忽略")
	}
	if calls != 3 || tools != 1 {
		t.Fatal("首次故障切换循环错误", calls, tools)
	}
	execute(t, f.pool, `DROP TRIGGER reject_routing_finish ON tombot.user_events`)
	execute(t, f.pool, `UPDATE tombot.jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
	if count, err := f.service.Recover(ctx, 1); err != nil || count != 1 {
		t.Fatal("恢复失败", err)
	}
	execute(t, f.pool, `UPDATE tombot.jobs SET available_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
	next := f.claim(t)
	run, err := worker.Process(ctx, next)
	if err != nil || run.Status != "succeeded" || calls != 3 || tools != 1 {
		t.Fatal("恢复重放了模型或工具", run.Status, calls, tools, err)
	}
}
