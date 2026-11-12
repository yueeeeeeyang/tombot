package dockernode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestJournalRejectsUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"symbolic-state", "symbolic-lock", "hard-state", "writable-state", "writable-lock"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newFixture(t)
			if _, err := fixture.node.Inspect(context.Background(), fixture.specification); err != nil {
				t.Fatal(err)
			}
			extension := ".json"
			if kind == "symbolic-lock" || kind == "writable-lock" {
				extension = ".lock"
			}
			path := filepath.Join(fixture.statePath, fixture.specification.UserID+extension)
			switch kind {
			case "symbolic-state", "symbolic-lock":
				target := path + ".original"
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Base(target), path); err != nil {
					t.Fatal(err)
				}
			case "hard-state":
				if err := os.Link(path, path+".other"); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Chmod(path, 0666); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := fixture.node.Ensure(context.Background(), fixture.specification); err == nil {
				t.Fatal("不安全状态文件被接受")
			}
			if fixture.engine.creates != 0 {
				t.Fatal("文件校验失败后仍修改资源")
			}
		})
	}
}
