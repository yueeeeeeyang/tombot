package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"io.yueyang/tom/internal/domain"
)

func fixture(t *testing.T) (*Files, string) {
	t.Helper()
	path := t.TempDir()
	for _, directory := range []string{"bots/first/out", "bots/second", "shared"} {
		if err := os.MkdirAll(filepath.Join(path, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	return f, path
}
func audited(context.Context, string, string) error { return nil }
func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *domain.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("错误码不符 %v，需要 %s", err, code)
	}
}

func TestFilesReadWriteEditAndAudit(t *testing.T) {
	f, _ := fixture(t)
	ctx := context.Background()
	var auditHash string
	before := func(_ context.Context, path, hash string) error {
		if path != "out/report.md" {
			t.Error("审计路径错误")
		}
		auditHash = hash
		return nil
	}
	first, err := f.Write(ctx, "first", "out/report.md", "一行\nsecond\n末行\n", false, before)
	if err != nil || auditHash != "" {
		t.Fatal(err)
	}
	window, err := f.Read(ctx, "first", "out/report.md", 2, 1)
	if err != nil || window.Content != "2\tsecond\n" || !window.Truncated || window.TotalLines != 3 {
		t.Fatal("读取窗口错误", window, err)
	}
	updated, err := f.Edit(ctx, "first", "out/report.md", "second", "二行", before)
	if err != nil || updated.OldSHA256 != first.SHA256 || auditHash != first.SHA256 {
		t.Fatal("旧内容审计丢失", err)
	}
	_, err = f.Edit(ctx, "first", "out/report.md", "不存在", "替换", before)
	requireCode(t, err, "EDIT_NOT_FOUND")
	_, err = f.Edit(ctx, "first", "out/report.md", "行", "替换", before)
	requireCode(t, err, "EDIT_NOT_UNIQUE")
	denied := errors.New("审计不可用")
	_, err = f.Write(ctx, "first", "out/report.md", "不应保存", false, func(context.Context, string, string) error { return denied })
	if !errors.Is(err, denied) {
		t.Fatal(err)
	}
	current, err := f.Read(ctx, "first", "out/report.md", 1, 0)
	if err != nil || current.SHA256 != updated.SHA256 {
		t.Fatal("审计失败仍修改文件", err)
	}
	_, err = f.Write(ctx, "first", "new/deep/report.md", "内容", false, audited)
	requireCode(t, err, "PARENT_MISSING")
	if _, err = f.Write(ctx, "first", "new/deep/report.md", "内容", true, audited); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceRejectsEscapesAndSpecialFiles(t *testing.T) {
	f, path := fixture(t)
	ctx := context.Background()
	outside := t.TempDir()
	for name, target := range map[string]string{"escape": outside, "other": filepath.Join(path, "bots/second")} {
		if err := os.Symlink(target, filepath.Join(path, "bots/first", name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"../second/private.txt", "shared/../bots/second/private.txt", filepath.Join(path, "bots/second/private.txt"), "escape/private.txt", "other/private.txt"} {
		if _, err := f.Read(ctx, "first", name, 1, 1); err == nil {
			t.Fatal("越界读取被接受", name)
		}
		if _, err := f.Write(ctx, "first", name, "内容", true, audited); err == nil {
			t.Fatal("越界写入被接受", name)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(path, "bots/first/pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := f.Read(ctx, "first", "pipe", 1, 1)
	requireCode(t, err, "NOT_TEXT_FILE")
	if err = os.WriteFile(filepath.Join(path, "bots/first/binary"), []byte{0, 1, 2}, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = f.Read(ctx, "first", "binary", 1, 1)
	requireCode(t, err, "NOT_TEXT_FILE")
	if err = os.Symlink("out", filepath.Join(path, "bots/first/local")); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(ctx, "first", "local/report.txt", "允许根内链接", false, audited); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Read(ctx, "first", filepath.Join(path, "bots/first/out/report.txt"), 1, 1); err != nil {
		t.Fatal(err)
	}
}

func TestFileTraversalSearchLimitsAndSharedPaths(t *testing.T) {
	f, _ := fixture(t)
	ctx := context.Background()
	for name, content := range map[string]string{"out/a.txt": "命中\n命中\n命中\n", "out/b.go": "package fixture", "node_modules/ignored.txt": "命中", "shared/report.txt": "共享命中"} {
		if _, err := f.Write(ctx, "first", name, content, true, audited); err != nil {
			t.Fatal(err)
		}
	}
	result, err := f.Search(ctx, "first", ".", "命中", 2)
	if err != nil || len(result.Matches) != 2 || !result.Truncated {
		t.Fatal("搜索边界错误", result, err)
	}
	for _, match := range result.Matches {
		if strings.Contains(match.Path, "node_modules") {
			t.Fatal("未忽略依赖目录")
		}
	}
	listing, err := f.Glob(ctx, "first", ".", "*.go")
	if err != nil || len(listing.Entries) != 1 || listing.Entries[0].Path != "out/b.go" {
		t.Fatal("递归匹配失败", listing, err)
	}
	shared, err := f.Search(ctx, "first", "shared", "共享", 0)
	if err != nil || len(shared.Matches) != 1 || shared.Matches[0].Path != "shared/report.txt" {
		t.Fatal("共享路径无法复用", shared, err)
	}
	_, err = f.Search(ctx, "first", ".", "[", 1)
	requireCode(t, err, "INVALID_PATTERN")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = f.List(canceled, "first", ".", 2)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("遍历取消未传播", err)
	}
	_, err = f.Write(ctx, "first", "out/large", strings.Repeat("x", MaxTextBytes+1), false, audited)
	requireCode(t, err, "FILE_TOO_LARGE")
}

func TestConcurrentExactEditsDoNotLoseUpdates(t *testing.T) {
	f, _ := fixture(t)
	ctx := context.Background()
	if _, err := f.Write(ctx, "first", "out/report", "original", false, audited); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var workers sync.WaitGroup
	for range 12 {
		workers.Go(func() {
			_, err := f.Edit(ctx, "first", "out/report", "original", "replacement", audited)
			if err == nil {
				successes.Add(1)
			} else {
				requireCode(t, err, "EDIT_NOT_FOUND")
			}
		})
	}
	workers.Wait()
	if successes.Load() != 1 {
		t.Fatal("并发精确编辑重复成功")
	}
}

func TestDescribeOutputRiskCannotFollowLinksAsLowRisk(t *testing.T) {
	f, path := fixture(t)
	for _, name := range []string{"out/report.md", "out/missing/report.md"} {
		target, output, err := f.Describe("first", name)
		if err != nil || !output || target != name {
			t.Fatal("普通产物路径识别失败", target, output, err)
		}
	}
	if err := os.Mkdir(filepath.Join(path, "bots/first/notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../notes", filepath.Join(path, "bots/first/out/link")); err != nil {
		t.Fatal(err)
	}
	_, output, err := f.Describe("first", "out/link/report.md")
	if err != nil || output {
		t.Fatal("链接提升规则未生效", err)
	}
	target, output, err := f.Describe("first", filepath.Join(path, "shared/report.md"))
	if err != nil || output || target != "shared/report.md" {
		t.Fatal("共享绝对路径归一化失败", target, err)
	}
}

func TestSavedToolOutputSupportsWindowReadingBeyondOrdinaryFileLimit(t *testing.T) {
	f, path := fixture(t)
	content := []byte(strings.Repeat("saved result line\n", 100000))
	result, err := f.SaveOutput(context.Background(), "first", "long-output", content, audited)
	if err != nil {
		t.Fatal(err)
	}
	read, err := f.Read(context.Background(), "first", result.Path, 99999, 2)
	if err != nil || read.TotalLines != 100000 || read.EndLine != 100000 || read.Truncated || read.SHA256 != result.SHA256 {
		t.Fatal("完整工具输出无法分页读取", read, err)
	}
	if err = os.WriteFile(filepath.Join(path, "bots/first/out/large"), content, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = f.Read(context.Background(), "first", "out/large", 1, 1)
	requireCode(t, err, "FILE_TOO_LARGE")
}

func TestCommandDirectoryAliasesAndRawOutputStorage(t *testing.T) {
	f, path := fixture(t)
	if err := os.Symlink("out", filepath.Join(path, "bots/first/output-alias")); err != nil {
		t.Fatal(err)
	}
	directory, err := f.Directory("first", "output-alias")
	if err != nil || directory != filepath.Join(path, "bots/first/out") {
		t.Fatal("命令目录未正确归一化", err, directory)
	}
	if _, err = f.Directory("first", "../second"); err == nil {
		t.Fatal("命令工作目录越界")
	}
	raw := []byte{0, 1, 255, 128, 10}
	result, err := f.SaveShellOutput(context.Background(), "first", "binary", string(raw), audited)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(path, "bots/first", result.Path))
	if err != nil || string(stored) != string(raw) {
		t.Fatal("二进制输出没有原样保存", err)
	}
	_, err = f.Read(context.Background(), "first", result.Path, 1, 1)
	requireCode(t, err, "NOT_TEXT_FILE")
}
