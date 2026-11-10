package provision

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
func shippedBrowserDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位测试文件")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "browser")
}

func TestShippedBrowserArchivesMatchPins(t *testing.T) {
	dir := shippedBrowserDir(t)
	if len(pinnedReleases) != 4 {
		t.Fatalf("受支持平台数量变化后要同时更新随仓库提供的包: %d", len(pinnedReleases))
	}
	for platform, release := range pinnedReleases {
		path := filepath.Join(dir, "chrome-"+release.Platform+".zip")
		source, err := materializeBrowserArchive(context.Background(), path)
		if err != nil {
			t.Fatalf("%s 不能把指针当成 zip: %v", platform, err)
		}
		digest, err := hashFile(source)
		if err != nil {
			t.Fatalf("%s 缺少随仓库提供的离线包 %s: %v", platform, source, err)
		}
		if digest != release.Checksum {
			t.Fatalf("%s 摘要不符，得到 %s", source, digest)
		}
	}
}

func TestEnsureBrowserInstallsShippedArchive(t *testing.T) {
	release, err := currentRelease()
	if err != nil {
		t.Skip("当前平台无固定浏览器资产")
	}
	pointer := filepath.Join(shippedBrowserDir(t), "chrome-"+release.Platform+".zip")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	if err = EnsureBrowser(ctx, dir, pointer); err != nil {
		t.Fatal(err)
	}
	if err = VerifyBrowser(dir); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureBrowserReadsLFSObjectInsteadOfPointer(t *testing.T) {
	release, err := currentRelease()
	if err != nil {
		t.Skip("当前平台无固定浏览器资产")
	}
	repo := t.TempDir()
	if err = exec.Command("git", "init", repo).Run(); err != nil {
		t.Fatal(err)
	}
	content := []byte("lfs-object-not-a-pointer")
	pointer := filepath.Join(repo, "browser", "chrome-"+release.Platform+".zip")
	if err = writeLFSFixture(repo, pointer, content); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(repo, "data")
	err = EnsureBrowser(context.Background(), data, "")
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatal("展开后的对象仍应接受摘要校验", err)
	}
	archive := filepath.Join(data, "runtime", "chrome-"+browserVersion+"-"+release.Platform+".zip")
	got, err := os.ReadFile(archive)
	if err != nil || string(got) != string(content) {
		t.Fatal("安装用了指针文本，没有读本地 LFS 对象", err)
	}
}

func TestLFSPointerWithoutObjectDoesNotFailChecksum(t *testing.T) {
	release, err := currentRelease()
	if err != nil {
		t.Skip("当前平台无固定浏览器资产")
	}
	repo := t.TempDir()
	if err = exec.Command("git", "init", repo).Run(); err != nil {
		t.Fatal(err)
	}
	pointer := filepath.Join(repo, "browser", "chrome-"+release.Platform+".zip")
	if err = os.MkdirAll(filepath.Dir(pointer), 0700); err != nil {
		t.Fatal(err)
	}
	text := "version https://git-lfs.github.com/spec/v1\noid sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\nsize 4\n"
	if err = os.WriteFile(pointer, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	err = EnsureBrowser(context.Background(), filepath.Join(repo, "data"), "")
	if err == nil || !strings.Contains(err.Error(), "指针") || strings.Contains(err.Error(), "SHA-256") || !lfsHintExplainsInstall(err.Error()) {
		t.Fatal("缺少对象时被当成摘要错误", err)
	}
}

func TestPointerErrorWhenGitLFSCommandMissing(t *testing.T) {
	release, err := currentRelease()
	if err != nil {
		t.Skip("当前平台无固定浏览器资产")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err = os.Symlink(gitPath, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	repo := t.TempDir()
	if err = exec.Command(gitPath, "init", repo).Run(); err != nil {
		t.Fatal(err)
	}
	pointer := filepath.Join(repo, "browser", "chrome-"+release.Platform+".zip")
	if err = os.MkdirAll(filepath.Dir(pointer), 0700); err != nil {
		t.Fatal(err)
	}
	text := "version https://git-lfs.github.com/spec/v1\noid sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\nsize 4\n"
	if err = os.WriteFile(pointer, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	err = EnsureBrowser(context.Background(), filepath.Join(repo, "data"), "")
	if err == nil || !strings.Contains(err.Error(), "没有 git-lfs") || !lfsHintExplainsInstall(err.Error()) {
		t.Fatal("未说明系统没有 git-lfs", err)
	}
}

func lfsHintExplainsInstall(message string) bool {
	install := strings.Index(message, "安装 Git LFS")
	pull := strings.Index(message, "git lfs pull")
	return install >= 0 && pull > install && strings.Contains(message, "brew install git-lfs") && strings.Contains(message, "git lfs install") && strings.Contains(message, "apt-get install git-lfs")
}

func writeLFSFixture(repo, pointer string, content []byte) error {
	sum := sha256.Sum256(content)
	oid := hex.EncodeToString(sum[:])
	object := filepath.Join(repo, ".git", "lfs", "objects", oid[:2], oid[2:4], oid)
	if err := os.MkdirAll(filepath.Dir(object), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(object, content, 0600); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(pointer), 0700); err != nil {
		return err
	}
	text := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, len(content))
	return os.WriteFile(pointer, []byte(text), 0600)
}

func TestLocateBrowserArchiveUsesShippedRepoCopy(t *testing.T) {
	release, err := currentRelease()
	if err != nil {
		t.Skip("当前平台无固定浏览器资产")
	}
	repoBrowser := shippedBrowserDir(t)
	data := filepath.Join(t.TempDir(), "bin", "data")
	exe := filepath.Join(filepath.Dir(repoBrowser), "bin", "tomd")
	source, err := locateBrowserArchive(data, exe, "", release, false)
	want := filepath.Join(repoBrowser, "chrome-"+release.Platform+".zip")
	if err != nil || source != want {
		t.Fatal("未使用仓库自带的离线包", source, err, want)
	}
	root := t.TempDir()
	sibling := defaultBrowserArchive(filepath.Join(root, "data"), release)
	if err = os.MkdirAll(filepath.Dir(sibling), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(sibling, []byte("local"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err = locateBrowserArchive(filepath.Join(root, "data"), exe, "", release, false)
	if err != nil || source != sibling {
		t.Fatal("数据目录同级的包应优先于仓库副本", source, err)
	}
}

func TestResolveBrowserArchiveUsesSiblingZip(t *testing.T) {
	release, err := currentRelease()
	if err != nil {
		t.Skip("当前平台无固定浏览器资产")
	}
	root := t.TempDir()
	data := filepath.Join(root, "data")
	def := defaultBrowserArchive(data+"/", release)
	want := filepath.Join(root, "browser", "chrome-"+release.Platform+".zip")
	if def != want {
		t.Fatalf("默认路径错误: %s", def)
	}
	if _, err = resolveBrowserArchive(data, "", release, false); err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "下载") || !strings.Contains(err.Error(), "TOM_BROWSER_ARCHIVE") {
		t.Fatal("缺少默认离线包时的提示不正确", err)
	}
	if source, err := resolveBrowserArchive(data, "", release, true); err != nil || source != "" {
		t.Fatal("已校验的运行时归档应直接复用", source, err)
	}
	if err = os.MkdirAll(filepath.Dir(def), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(def, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = resolveBrowserArchive(data, "", release, false); err == nil || !strings.Contains(err.Error(), "普通文件") {
		t.Fatal("目录被当成离线包", err)
	}
	if err = os.Remove(def); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(def, []byte("official"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err := resolveBrowserArchive(data, "", release, true)
	if err != nil || source != def {
		t.Fatal("默认离线包应优先于已有缓存", source, err)
	}
	override := filepath.Join(root, "override.zip")
	if err = os.WriteFile(override, []byte("override"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err = resolveBrowserArchive(data, override, release, true)
	if err != nil || source != override {
		t.Fatal("覆盖路径未生效", source, err)
	}
}

func TestEnsureBrowserInstallsDefaultZipWithoutDownload(t *testing.T) {
	release, err := currentRelease()
	if err != nil {
		t.Skip("当前平台无固定浏览器资产")
	}
	root := t.TempDir()
	data := filepath.Join(root, "data")
	def := defaultBrowserArchive(data, release)
	if err = os.MkdirAll(filepath.Dir(def), 0700); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(archiveFixture(t, "chrome/file"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(def, payload, 0600); err != nil {
		t.Fatal(err)
	}
	err = EnsureBrowser(context.Background(), data, "")
	if err == nil || !strings.Contains(err.Error(), "SHA-256") || strings.Contains(err.Error(), "下载") {
		t.Fatal("未按默认离线包安装", err)
	}
	copied, err := os.ReadFile(filepath.Join(data, "runtime", "chrome-"+browserVersion+"-"+release.Platform+".zip"))
	if err != nil || string(copied) != string(payload) {
		t.Fatal("运行时目录没有默认离线包的副本", err)
	}
	override := filepath.Join(root, "override.zip")
	overridePayload := []byte("override-archive")
	if err = os.WriteFile(override, overridePayload, 0600); err != nil {
		t.Fatal(err)
	}
	if err = EnsureBrowser(context.Background(), data, override); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatal("覆盖路径未参与校验", err)
	}
	copied, err = os.ReadFile(filepath.Join(data, "runtime", "chrome-"+browserVersion+"-"+release.Platform+".zip"))
	if err != nil || string(copied) != string(overridePayload) {
		t.Fatal("运行时目录仍是默认包，覆盖未生效", err)
	}
}

func TestEnsureBrowserMissingZipDoesNotLeaveDownload(t *testing.T) {
	release, err := currentRelease()
	if err != nil {
		t.Skip("当前平台无固定浏览器资产")
	}
	data := filepath.Join(t.TempDir(), "data")
	archive := filepath.Join(data, "runtime", "chrome-"+browserVersion+"-"+release.Platform+".zip")
	if err = os.MkdirAll(filepath.Dir(archive), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(archive, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	err = EnsureBrowser(context.Background(), data, "")
	if err == nil || strings.Contains(err.Error(), "下载") || !strings.Contains(err.Error(), defaultBrowserArchive(data, release)) {
		t.Fatal("损坏缓存触发了下载或未指出默认路径", err)
	}
	partial, statErr := os.Stat(archive + ".part")
	if statErr == nil {
		t.Fatal("缺少离线包时写出了下载分片", partial)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = EnsureBrowser(ctx, data, ""); !errors.Is(err, context.Canceled) {
		t.Fatal("取消未生效", err)
	}
}

func TestEnsureBrowserDoesNotTruncateArchiveUsedAsSource(t *testing.T) {
	release, err := currentRelease()
	if err != nil {
		t.Skip("当前平台无固定浏览器资产")
	}
	data := filepath.Join(t.TempDir(), "data")
	archive := filepath.Join(data, "runtime", "chrome-"+browserVersion+"-"+release.Platform+".zip")
	if err = os.MkdirAll(filepath.Dir(archive), 0700); err != nil {
		t.Fatal(err)
	}
	content := []byte("keep-this-archive")
	if err = os.WriteFile(archive, content, 0600); err != nil {
		t.Fatal(err)
	}
	if err = EnsureBrowser(context.Background(), data, archive); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatal("把运行时归档当作来源时未做校验", err)
	}
	got, err := os.ReadFile(archive)
	if err != nil || string(got) != string(content) {
		t.Fatal("来源与目标是同一文件时内容被截断", err)
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
