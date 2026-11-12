package provision

import (
	"context"
	"errors"
	"testing"
	"time"

	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/testkit"
)

func TestVerifyRepairAndReadiness(t *testing.T) {
	db := testkit.Database(t)
	events := bus.New(db)
	ready := false
	runs := 0
	step := Step{ID: "dependency", Title: "依赖", Phase: "preparing", Blocking: true, Verify: func(context.Context) error {
		if !ready {
			return errors.New("依赖缺失")
		}
		return nil
	}, Run: func(context.Context) error { runs++; ready = true; return nil }}
	p, err := New(context.Background(), db, events, []Step{step}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if p.Ready() || !p.Claim() || p.Claim() {
		t.Fatal("引导互斥错误")
	}
	if err = p.RunClaimed(context.Background(), false); err != nil || !p.Ready() {
		t.Fatalf("引导失败: %v", err)
	}
	if !p.Claim() {
		t.Fatal("不能重跑")
	}
	if err = p.RunClaimed(context.Background(), false); err != nil || runs != 1 {
		t.Fatalf("依赖已满足仍重复安装: %d %v", runs, err)
	}
}
func TestBlockingFailureTimeoutAndDiagnostics(t *testing.T) {
	for _, diagnose := range []bool{true, false} {
		t.Run(map[bool]string{true: "诊断", false: "安装"}[diagnose], func(t *testing.T) {
			db := testkit.Database(t)
			runs := 0
			steps := []Step{{ID: "blocked", Phase: "selfcheck", Blocking: true, Verify: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, Run: func(context.Context) error { runs++; return errors.New("安装失败") }}}
			p, err := New(context.Background(), db, bus.New(db), steps, 5*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			if !p.Claim() {
				t.Fatal("无法领取")
			}
			if err = p.RunClaimed(context.Background(), diagnose); err == nil || p.Ready() || p.Snapshot().Phase != "failed" {
				t.Fatal("失败错误宣告就绪")
			}
			if diagnose && runs != 0 {
				t.Fatal("诊断执行了修改")
			}
		})
	}
}
func TestNonBlockingCapabilityDegrades(t *testing.T) {
	db := testkit.Database(t)
	p, err := New(context.Background(), db, bus.New(db), []Step{{ID: "optional", Phase: "selfcheck", Verify: func(context.Context) error { return errors.New("缺少可选工具") }}}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Claim() {
		t.Fatal("无法领取")
	}
	if err = p.RunClaimed(context.Background(), false); err != nil || !p.Ready() || p.Snapshot().Steps[0].Status != "degraded" {
		t.Fatal("可选能力处理错误")
	}
}
func TestPackageCommands(t *testing.T) {
	for _, manager := range []string{"apt-get", "dnf", "yum", "apk", "zypper"} {
		for _, fonts := range []bool{true, false} {
			name, args, err := PackageCommand(manager, fonts)
			if err != nil || name != manager || len(args) < 3 {
				t.Fatalf("包映射失败: %s %v", manager, err)
			}
		}
	}
	if _, _, err := PackageCommand("unknown", false); err == nil {
		t.Fatal("未知包管理器被接受")
	}
}
func TestRealPTY(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := checkPTY(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnoseReportsAllFailuresWithoutInstalling(t *testing.T) {
	runs := 0
	steps := []Step{}
	for _, id := range []string{"first", "second"} {
		steps = append(steps, Step{ID: id, Blocking: true, Verify: func(context.Context) error { return errors.New("缺少依赖") }, Run: func(context.Context) error { runs++; return nil }})
	}
	state, err := Diagnose(context.Background(), steps, time.Second)
	if err == nil || state.Phase != "failed" || len(state.Steps) != 2 || runs != 0 {
		t.Fatal("只读诊断未正确汇总失败")
	}
}
