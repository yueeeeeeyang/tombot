package provision

import (
	"archive/zip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func archiveFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	entry, err := writer.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = entry.Write([]byte("browser")); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestArchiveRejectsPathEscape(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "nested/../../escape"} {
		t.Run(name, func(t *testing.T) {
			if _, err := extractZip(archiveFixture(t, name), t.TempDir()); err == nil {
				t.Fatal("归档路径逃逸未被拒绝")
			}
		})
	}
	manifest, err := extractZip(archiveFixture(t, "chrome/file"), t.TempDir())
	if err != nil || len(manifest) != 1 {
		t.Fatalf("合法归档失败: %v", err)
	}
}

func TestResolvedArchiveRootAcceptsSymlinkedParentButRejectsEscape(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	if err := os.Mkdir(realRoot, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err = checkResolvedPath(alias, filepath.Join(canonical, "nested")); err != nil {
		t.Fatal("真实根目录内路径误判越界", err)
	}
	if err = checkResolvedPath(alias, filepath.Dir(canonical)); err == nil {
		t.Fatal("真实根目录外路径被接受")
	}
}
func TestOfflineChecksumRejectsModifiedArchive(t *testing.T) {
	if _, err := currentRelease(); err != nil {
		t.Skip("当前平台无固定浏览器资产")
	}
	if err := EnsureBrowser(context.Background(), t.TempDir(), archiveFixture(t, "chrome/file")); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("错误资产被接受: %v", err)
	}
}
func TestDownloadResumeAndRangeValidation(t *testing.T) {
	content := "download-content"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=5-" {
			t.Error("未发送续传区间")
		}
		w.Header().Set("Content-Range", "bytes 5-15/16")
		w.WriteHeader(206)
		if _, err := w.Write([]byte(content[5:])); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "archive")
	if err := os.WriteFile(path+".part", []byte(content[:5]), 0600); err != nil {
		t.Fatal(err)
	}
	if err := downloadAttempt(context.Background(), server.Client(), server.URL, path); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != content {
		t.Fatal("续传内容错误")
	}
}
func TestDownloadHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := download(ctx, "http://127.0.0.1:1", filepath.Join(t.TempDir(), "archive")); err == nil {
		t.Fatal("取消未生效")
	}
}
func TestRealBrowser(t *testing.T) {
	archive := os.Getenv("TOM_TEST_BROWSER_ARCHIVE")
	if archive == "" {
		t.Skip("未设置 TOM_TEST_BROWSER_ARCHIVE，跳过真实浏览器；发布验收必须运行")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	if err := EnsureBrowser(ctx, dir, archive); err != nil {
		t.Fatal(err)
	}
	if err := VerifyBrowser(dir); err != nil {
		t.Fatal(err)
	}
	if err := CheckBrowser(ctx, BrowserPath(dir)); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveRejectsSymlinkChainEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.WriteFile(outside, []byte("protected"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "links.zip")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for _, link := range []struct{ name, target string }{{"a", "."}, {"b", "a/../outside"}} {
		header := &zip.FileHeader{Name: link.name}
		header.SetMode(os.ModeSymlink | 0777)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = entry.Write([]byte(link.target)); err != nil {
			t.Fatal(err)
		}
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, "stage")
	if err = os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = extractZip(archive, stage); err == nil {
		t.Fatal("链接链逃逸未被拒绝")
	}
}
