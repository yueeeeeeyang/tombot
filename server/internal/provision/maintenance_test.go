package provision

import (
	"context"
	"errors"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/testkit"
)

// 验证领取维护权、清除旧能力、审计与事件要么一起成功，要么可安全重试。
func TestMaintenanceAtomicResetAndRepair(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	events := bus.New(db)
	valid := true
	repairs := 0
	step := Step{ID: "font", Title: "字形", Phase: "selfcheck", Blocking: true,
		Verify: func(context.Context) error {
			if !valid {
				return errors.New("缺少字形")
			}
			return nil
		}, Run: func(context.Context) error { repairs++; valid = true; return nil },
	}
	p, err := New(ctx, db, events, []Step{step}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Claim() {
		t.Fatal("无法领取首次引导")
	}
	if err = p.RunClaimed(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO system_state VALUES('keep','{}',0)"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TRIGGER fail_reset BEFORE INSERT ON events WHEN NEW.kind='bootstrap.step' BEGIN SELECT RAISE(ABORT,'event unavailable'); END"); err != nil {
		t.Fatal(err)
	}
	before, err := p.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wake := events.Wake()
	if err := p.ClaimMaintenance(ctx, "setup", true); err == nil || !p.Ready() {
		t.Fatal("失败重置改变就绪状态")
	}
	select {
	case <-wake:
		t.Fatal("回滚唤醒事件流")
	default:
	}
	var capabilities, audits, preserved int
	if err = db.QueryRow("SELECT (SELECT COUNT(*) FROM capabilities),(SELECT COUNT(*) FROM audit_log),(SELECT COUNT(*) FROM system_state WHERE key='keep')").Scan(&capabilities, &audits, &preserved); err != nil || capabilities != 1 || audits != 0 || preserved != 1 {
		t.Fatal("回滚破坏既有记录", capabilities, audits, preserved, err)
	}
	if _, err = db.Exec("DROP TRIGGER fail_reset"); err != nil {
		t.Fatal(err)
	}
	for _, reset := range []bool{false, true} {
		if err := p.ClaimMaintenance(ctx, "setup", reset); err != nil {
			t.Fatal(err)
		}
		if err = db.QueryRow("SELECT COUNT(*) FROM capabilities").Scan(&capabilities); err != nil || (capabilities == 0) != reset {
			t.Fatal("修复与重置未正确保留或清除能力", capabilities, err)
		}
		var conflict *domain.Error
		if !errors.As(p.ClaimMaintenance(ctx, "setup", !reset), &conflict) || conflict.Code != "BOOTSTRAP_RUNNING" {
			t.Fatal("并发维护未拒绝")
		}
		status, err := p.Status(ctx)
		if err != nil || status.Cursor <= before.Cursor || len(status.Steps) != 0 || p.Ready() {
			t.Fatal("重置后游标或门禁错误", status, err)
		}
		valid = !reset
		if err := p.RunClaimed(ctx, false); err != nil || !p.Ready() {
			t.Fatal("无法恢复就绪", err)
		}
	}
	if repairs != 1 {
		t.Fatal("重复安装已满足的依赖", repairs)
	}
	if err := db.QueryRow("SELECT (SELECT COUNT(*) FROM audit_log WHERE actor='setup' AND action IN ('bootstrap.repair','bootstrap.reset')),(SELECT COUNT(*) FROM system_state WHERE key='keep')").Scan(&audits, &preserved); err != nil || audits != 2 || preserved != 1 {
		t.Fatal("维护审计或业务记录错误", audits, preserved, err)
	}
}

func TestReadyAndCapabilityNeverPublishedBeforeCommit(t *testing.T) {
	for _, reject := range []string{"ok", "ready"} {
		t.Run(reject, func(t *testing.T) {
			ctx := context.Background()
			db := testkit.Database(t)
			events := bus.New(db)
			step := Step{ID: "check", Phase: "selfcheck", Blocking: true, Verify: func(context.Context) error { return nil }}
			p, err := New(ctx, db, events, []Step{step}, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			condition := "json_extract(NEW.payload_json,'$.steps[0].status')='ok'"
			if reject == "ready" {
				condition = "json_extract(NEW.payload_json,'$.phase')='ready'"
			}
			if _, err = db.Exec("CREATE TRIGGER fail_state BEFORE INSERT ON events WHEN NEW.kind='bootstrap.step' AND " + condition + " BEGIN SELECT RAISE(ABORT,'state unavailable'); END"); err != nil {
				t.Fatal(err)
			}
			if !p.Claim() {
				t.Fatal("无法领取执行权")
			}
			if err = p.RunClaimed(ctx, false); err == nil || p.Ready() {
				t.Fatal("未提交状态开放业务")
			}
			var phase string
			var count int
			if err = db.QueryRow("SELECT json_extract(value_json,'$.phase'),(SELECT COUNT(*) FROM capabilities) FROM system_state WHERE key='bootstrap'").Scan(&phase, &count); err != nil || phase == "ready" || (count == 1) != (reject == "ready") {
				t.Fatal("状态与事件事务未回滚", phase, count, err)
			}
		})
	}
}

func TestSupportedChineseFontMatchesInstallAndRender(t *testing.T) {
	for family, expected := range map[string]bool{"Noto Sans": false, "DejaVu Sans": false, "WenQuanYi Zen Hei": false, "Noto Sans CJK SC": true, "Noto Serif SC": true, "Source Han Sans CN": true, "PingFang SC": true} {
		if supportsChineseFont(family) != expected {
			t.Fatal("字体准备与渲染支持范围错误", family)
		}
	}
}

func TestWaitReadyDelaysScheduledWorkUntilRepairFinishes(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	events := bus.New(db)
	p, err := New(ctx, db, events, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := p.ClaimMaintenance(ctx, "system", false); err != nil {
			t.Fatal(err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := p.WaitReady(canceled); !errors.Is(err, context.Canceled) {
			t.Fatal("关闭后仍等待引导", err)
		}
		resumed := make(chan error, 1)
		go func() { resumed <- p.WaitReady(ctx) }()
		select {
		case <-resumed:
			t.Fatal("未就绪时发放调度名额")
		case <-time.After(10 * time.Millisecond):
		}
		if err := p.RunClaimed(ctx, false); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-resumed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("就绪后调度未恢复")
		}
	}
}
