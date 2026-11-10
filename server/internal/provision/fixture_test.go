package provision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/testkit"
)

func TestFixtureFailsOnceThenSucceedsOnRetry(t *testing.T) {
	steps := loadFixture(t, `{
		"steps": [
			{"id":"detect_os","title":"检测操作系统","phase":"detecting"},
			{"id":"ensure_browser","title":"准备托管浏览器","phase":"preparing","fail_first":true,"detail":"夹具没有浏览器"}
		]
	}`)
	db := testkit.Database(t)
	provisioner, err := New(context.Background(), db, bus.New(db), steps, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !provisioner.Claim() {
		t.Fatal("无法领取引导")
	}
	if err = provisioner.RunClaimed(context.Background(), false); err == nil || provisioner.Ready() {
		t.Fatal("夹具第一次不应装好浏览器", err)
	}
	state := provisioner.Snapshot()
	if state.Phase != "failed" || len(state.Steps) != 2 || state.Steps[0].Status != "ok" || state.Steps[1].Status != "failed" || state.Error != "夹具没有浏览器" {
		t.Fatalf("失败进度不正确: %+v", state)
	}
	if !provisioner.Claim() {
		t.Fatal("失败后无法重试")
	}
	if err = provisioner.RunClaimed(context.Background(), false); err != nil || !provisioner.Ready() {
		t.Fatal("重试应通过且不下载浏览器", err)
	}
}

func TestFixtureReportsRunningBeforeItFinishes(t *testing.T) {
	steps := loadFixture(t, `{
		"steps": [
			{"id":"install_fonts","title":"安装中文字体","phase":"preparing","delay_ms":300}
		]
	}`)
	db := testkit.Database(t)
	provisioner, err := New(context.Background(), db, bus.New(db), steps, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !provisioner.Claim() {
		t.Fatal("无法领取引导")
	}
	done := make(chan error, 1)
	go func() { done <- provisioner.RunClaimed(context.Background(), false) }()
	deadline := time.Now().Add(2 * time.Second)
	sawRunning := false
	for time.Now().Before(deadline) {
		for _, step := range provisioner.Snapshot().Steps {
			if step.ID == "install_fonts" && step.Status == "running" {
				sawRunning = true
			}
		}
		if sawRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawRunning {
		t.Fatalf("没有观察到进行中的步骤: %+v", provisioner.Snapshot())
	}
	if err = <-done; err != nil || !provisioner.Ready() {
		t.Fatal("延迟步骤应完成", err)
	}
}

func TestFixtureRejectsMalformedScripts(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"steps":[]}`,
		`{"steps":[{"id":"detect_os","title":"检测操作系统","phase":"detecting"}],"extra":true}`,
		`{"steps":[{"id":"detect_os","title":"检测操作系统","phase":"ready"}]}`,
		`{"steps":[{"id":"detect_os","title":"检测操作系统","phase":"detecting","fail_first":true}]}`,
		`{"steps":[{"id":"detect_os","title":"检测操作系统","phase":"detecting"},{"id":"detect_os","title":"再来一次","phase":"detecting"}]}`,
	} {
		path := filepath.Join(t.TempDir(), "fixture.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFixture(path); err == nil {
			t.Fatalf("非法夹具被接受: %s", body)
		}
	}
	if _, err := LoadFixture(filepath.Join(t.TempDir(), "missing.json")); err == nil || !strings.Contains(err.Error(), "无法读取") {
		t.Fatal("缺失夹具应失败", err)
	}
}

func loadFixture(t *testing.T, body string) []Step {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	steps, err := LoadFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	return steps
}
