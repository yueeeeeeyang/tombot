package main

import (
	"os"
	"path/filepath"
	"testing"

	"io.yueyang/tom/internal/config"
)

func TestCompanionVariableDoesNotSkipFontsOrBrowser(t *testing.T) {
	t.Setenv("TOM_DESKTOP_COMPANION", "1")
	t.Setenv("TOM_BOOTSTRAP_FIXTURE", "")
	steps, err := selectBootstrapSteps(config.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, step := range steps {
		if step.Run != nil {
			found[step.ID] = true
		}
	}
	if !found["install_fonts"] || !found["ensure_browser"] {
		t.Fatalf("完整引导缺少字体或浏览器安装: %+v", found)
	}
}

func TestFixtureReplacesBootstrapSteps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.json")
	if err := os.WriteFile(path, []byte(`{"steps":[{"id":"detect_os","title":"检测操作系统","phase":"detecting"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOM_DESKTOP_COMPANION", "1")
	t.Setenv("TOM_BOOTSTRAP_FIXTURE", path)
	steps, err := selectBootstrapSteps(config.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].ID != "detect_os" || steps[0].Run != nil {
		t.Fatalf("夹具不应安装字体或浏览器: %+v", steps)
	}
}
