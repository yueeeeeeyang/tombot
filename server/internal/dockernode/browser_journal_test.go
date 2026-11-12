package dockernode

import (
	"os"
	"path/filepath"
	"testing"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/infra/ids"
)

func TestNodeBrowserJournalRejectsRebindingAndRegression(t *testing.T) {
	f, b, _, r, current, _ := newBrowserFixture(t)
	if err := b.ensure(t.Context(), r, current); err != nil {
		t.Fatal(err)
	}
	active, err := f.node.journal.readBrowser(r.Specification.UserID)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*browserRecord){
		func(s *browserRecord) { s.Phase = "prepared" }, func(s *browserRecord) { s.Phase = "closed"; s.Closure = "agent" },
		func(s *browserRecord) { s.Grant.Revision++ }, func(s *browserRecord) { s.Grant.Limits.Width-- },
		func(s *browserRecord) { s.Specification.FencingToken++ }, func(s *browserRecord) { s.Grant.Request.Group.ID = ids.New() },
		func(s *browserRecord) { s.Closure = "isolated" }, func(s *browserRecord) { s.Version = 2 },
	} {
		invalid := *active
		change(&invalid)
		if f.node.journal.saveBrowser(&invalid) == nil {
			t.Fatal("浏览器记录接受改绑或回退", invalid)
		}
	}
	if err := b.retire(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	closed, err := f.node.journal.readBrowser(r.Specification.UserID)
	if err != nil {
		t.Fatal(err)
	}
	invalid := *closed
	invalid.Closure = "isolated"
	if f.node.journal.saveBrowser(&invalid) == nil {
		t.Fatal("关闭方式被改写")
	}
	invalid = *closed
	invalid.Phase = "prepared"
	invalid.Closure = ""
	invalid.Grant.Request.Group.ID = ids.New()
	if f.node.journal.saveBrowser(&invalid) == nil {
		t.Fatal("最高消费版本被复用")
	}
	invalid = *closed
	invalid.Phase = "prepared"
	invalid.Closure = ""
	invalid.Grant.Revision++
	if f.node.journal.saveBrowser(&invalid) == nil {
		t.Fatal("服务实例被复用")
	}
}

func TestPreBrowserLifecycleVersionCannotOperate(t *testing.T) {
	f := newFixture(t)
	if _, err := f.node.Ensure(t.Context(), f.specification); err != nil {
		t.Fatal(err)
	}
	state, err := f.node.journal.read(f.specification.UserID)
	if err != nil {
		t.Fatal(err)
	}
	state.Version = 3
	encoded, err := agentchannel.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.statePath, f.specification.UserID+".json")
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.node.Ensure(t.Context(), f.specification); err == nil {
		t.Fatal("旧生命周期继续执行")
	}
	retained, err := os.ReadFile(path)
	if err != nil || string(retained) != string(encoded) {
		t.Fatal("旧记录被原地转换", err)
	}
}

func TestNodeBrowserOptionsRejectUnboundedBudgets(t *testing.T) {
	for _, change := range []func(*BrowserOptions){func(o *BrowserOptions) { o.CallTimeout = 0 }, func(o *BrowserOptions) { o.CleanupTimeout = o.CallTimeout }, func(o *BrowserOptions) { o.ShutdownTimeout = o.CleanupTimeout - 1 }, func(o *BrowserOptions) { o.Limits.MaximumWindows = 0 }, func(o *BrowserOptions) { o.Groups.MaximumProcesses = 0 }} {
		options := testBrowserOptions()
		change(&options)
		if options.Validate() == nil {
			t.Fatal("接受非法浏览器预算")
		}
	}
}

func TestNodeBrowserRecordParticipatesInStartupRecovery(t *testing.T) {
	f, b, _, r, current, _ := newBrowserFixture(t)
	if err := b.ensure(t.Context(), r, current); err != nil {
		t.Fatal(err)
	}
	if err := b.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.node.restoreEgress(t.Context()); err != nil {
		t.Fatalf("浏览器恢复后无法继续恢复出网：%+v", err)
	}
}
