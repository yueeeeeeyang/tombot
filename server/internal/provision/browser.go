package provision

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const browserVersion = "157.0.8103.38"
const maxArchiveBytes int64 = 512 << 20
const maxExtractBytes int64 = 2 << 30

type release struct{ Platform, Checksum, Executable string }

func (r release) archiveURL() string {
	return "https://storage.googleapis.com/chrome-for-testing-public/" + browserVersion + "/" + r.Platform + "/chrome-" + r.Platform + ".zip"
}

// 摘要在纳入版本时按官方 zip 计算；升级必须同时更新版本与校验值。运行时只接受这份离线包。
var pinnedReleases = map[string]release{
	"linux/amd64":  {"linux64", "8cdeb344e9b2cefe59f4aea8d1d93d5d93054c03cde217665c16fc1f62e72a66", "chrome-linux64/chrome"},
	"linux/arm64":  {"linux-arm64", "5517510e61657ff43ee9ab804ecd1a499b3d5bd3caf72f097516bd6565ba5a17", "chrome-linux-arm64/chrome"},
	"darwin/amd64": {"mac-x64", "c6fcc2869b0d5a421e880d7bcdabe96e4261c4100e125fc4e4daf6346474efa6", "chrome-mac-x64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"},
	"darwin/arm64": {"mac-arm64", "3406b38b52b7d341d74450eb7b369be038c9fec9894c8b8a9e9e9f97333caf2f", "chrome-mac-arm64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"},
}

func currentRelease() (release, error) {
	r, ok := pinnedReleases[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return r, fmt.Errorf("当前平台没有经过校验的浏览器资产")
	}
	return r, nil
}
func BrowserPath(dir string) string {
	r, ok := pinnedReleases[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return ""
	}
	return filepath.Join(dir, "runtime", "browser", filepath.FromSlash(r.Executable))
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return "", errors.Join(copyErr, closeErr)
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

// VerifyBrowser 同时检查归档摘要与展开后的所有文件，避免缓存损坏被误判就绪。
func VerifyBrowser(dir string) error {
	r, err := currentRelease()
	if err != nil {
		return err
	}
	archive := filepath.Join(dir, "runtime", "chrome-"+browserVersion+"-"+r.Platform+".zip")
	digest, err := hashFile(archive)
	if err != nil {
		return err
	}
	if digest != r.Checksum {
		return fmt.Errorf("浏览器归档校验失败")
	}
	encoded, err := os.ReadFile(filepath.Join(dir, "runtime", "browser-manifest.json"))
	if err != nil {
		return err
	}
	var manifest map[string]string
	if err = json.Unmarshal(encoded, &manifest); err != nil {
		return err
	}
	if len(manifest) == 0 {
		return fmt.Errorf("浏览器文件清单为空")
	}
	root := filepath.Join(dir, "runtime", "browser")
	for relative, expected := range manifest {
		path, err := containedPath(root, relative)
		if err != nil {
			return err
		}
		resolved, resolveErr := filepath.EvalSymlinks(path)
		if resolveErr != nil {
			return resolveErr
		}
		if err := checkResolvedPath(root, resolved); err != nil {
			return err
		}
		stat, err := os.Lstat(path)
		if err != nil {
			return err
		}
		var actual string
		if stat.Mode()&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(path)
			if readErr != nil {
				return readErr
			}
			actual = "link:" + target
		} else {
			actual, err = hashFile(path)
			if err != nil {
				return err
			}
		}
		if actual != expected {
			return fmt.Errorf("浏览器文件校验失败")
		}
	}
	stat, err := os.Stat(BrowserPath(dir))
	if err != nil {
		return err
	}
	if stat.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("浏览器不可执行")
	}
	return nil
}

// defaultBrowserArchive 与数据目录同级，避免把安装源放进会整目录备份的 runtime。
// 发布时二进制常在 /usr/local/bin，数据在 /var/lib/tom/data，不能跟可执行文件放在一起。
func defaultBrowserArchive(dataDir string, r release) string {
	return filepath.Join(filepath.Dir(filepath.Clean(dataDir)), "browser", "chrome-"+r.Platform+".zip")
}

// browserArchiveCandidates 先看数据目录同级，再看可执行文件旁边和它的上一级。
// 开发构建的 tomd 在 server/bin，随仓库的四个平台包在 server/browser，用上一级才能直接找到。
func browserArchiveCandidates(dataDir, executable string, r release) []string {
	name := "chrome-" + r.Platform + ".zip"
	raw := []string{defaultBrowserArchive(dataDir, r)}
	if executable != "" {
		dir := filepath.Dir(filepath.Clean(executable))
		raw = append(raw, filepath.Join(dir, "browser", name), filepath.Join(filepath.Dir(dir), "browser", name))
	}
	seen := map[string]bool{}
	paths := make([]string, 0, len(raw))
	for _, path := range raw {
		path = filepath.Clean(path)
		if seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	return paths
}

// locateBrowserArchive 决定本次安装读哪一份本地 zip。override 对应 TOM_BROWSER_ARCHIVE。
// 候选路径都不存在时，只有运行时目录里已经校验通过的归档可以继续用，例如备份还原之后。
func locateBrowserArchive(dataDir, executable, override string, r release, cachedValid bool) (string, error) {
	if override != "" {
		return override, nil
	}
	candidates := browserArchiveCandidates(dataDir, executable, r)
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err == nil {
			if !info.Mode().IsRegular() {
				return "", fmt.Errorf("官方离线浏览器包必须是普通文件：%s", path)
			}
			return path, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
	}
	if cachedValid {
		return "", nil
	}
	return "", fmt.Errorf("未找到官方离线浏览器包 %s。各平台包已随仓库放在 server/browser/chrome-%s.zip（Git LFS）。请在仓库根目录执行 git lfs pull；数据目录不在仓库内时，把该文件复制到上述路径。需要改位置时设置 TOM_BROWSER_ARCHIVE", candidates[0], r.Platform)
}

// resolveBrowserArchive 用当前可执行文件补齐仓库内的默认包位置。
func resolveBrowserArchive(dataDir, override string, r release, cachedValid bool) (string, error) {
	executable, _ := os.Executable()
	return locateBrowserArchive(dataDir, executable, override, r, cachedValid)
}

// materializeBrowserArchive 在文件是 Git LFS 指针时改为读取本地对象。
// 对象就在仓库的 .git/lfs/objects 里，不访问浏览器分发地址。
func materializeBrowserArchive(ctx context.Context, path string) (string, error) {
	oid, size, ok := parseLFSPointer(path)
	if !ok {
		return path, nil
	}
	object, err := localLFSObject(ctx, path, oid)
	if err != nil {
		return "", fmt.Errorf("浏览器包 %s 是 Git LFS 指针，无法读取本地对象：%w", path, err)
	}
	info, err := os.Stat(object)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("浏览器包 %s 仍是 Git LFS 指针，本地没有对象 %s。请在仓库根目录执行 git lfs pull", path, oid)
		}
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return "", fmt.Errorf("浏览器包 %s 的 Git LFS 对象不完整", path)
	}
	return object, nil
}

func parseLFSPointer(path string) (oid string, size int64, ok bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 1024 {
		return "", 0, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", 0, false
	}
	if !strings.HasPrefix(string(raw), "version https://git-lfs.github.com/spec/v1\n") && !strings.HasPrefix(string(raw), "version https://git-lfs.github.com/spec/v1\r\n") {
		return "", 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if value, found := strings.CutPrefix(line, "oid sha256:"); found {
			oid = value
		}
		if value, found := strings.CutPrefix(line, "size "); found && size == 0 {
			size, err = strconv.ParseInt(value, 10, 64)
			if err != nil {
				return "", 0, false
			}
		}
	}
	if len(oid) != 64 || size <= 0 {
		return "", 0, false
	}
	for _, char := range oid {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return "", 0, false
		}
	}
	return oid, size, true
}

func localLFSObject(ctx context.Context, file, oid string) (string, error) {
	command := exec.CommandContext(ctx, "git", "rev-parse", "--path-format=absolute", "--git-path", "lfs/objects")
	command.Dir = filepath.Dir(file)
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	base := strings.TrimSpace(string(output))
	if base == "" || len(oid) < 4 {
		return "", fmt.Errorf("无法定位 LFS 对象目录")
	}
	return filepath.Join(base, oid[:2], oid[2:4], oid), nil
}

func copyBrowserArchive(source, archive string) error {
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("无法读取官方离线浏览器包 %s：%w", source, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("官方离线浏览器包必须是普通文件：%s", source)
	}
	if destInfo, statErr := os.Stat(archive); statErr == nil {
		// 覆盖路径直接指着运行时归档时，先截断会毁掉唯一副本。
		if os.SameFile(info, destInfo) {
			return nil
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	file, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("无法读取官方离线浏览器包 %s：%w", source, err)
	}
	dest, err := os.OpenFile(archive, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return errors.Join(err, file.Close())
	}
	written, copyErr := io.Copy(dest, io.LimitReader(file, maxArchiveBytes+1))
	if err = errors.Join(copyErr, dest.Close(), file.Close()); err != nil {
		return err
	}
	if written > maxArchiveBytes {
		return fmt.Errorf("浏览器归档超过大小上限")
	}
	return nil
}

// EnsureBrowser 用官方离线 zip 安装托管浏览器：复制到运行时目录、核对固定摘要，再在临时目录展开并替换。
// offline 非空时改用该路径；否则读取仓库自带或数据目录同级的包。不会从网络下载。
func EnsureBrowser(ctx context.Context, dir, offline string) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	r, err := currentRelease()
	if err != nil {
		return err
	}
	root := filepath.Join(dir, "runtime")
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	archive := filepath.Join(root, "chrome-"+browserVersion+"-"+r.Platform+".zip")
	digest, hashErr := hashFile(archive)
	source, err := resolveBrowserArchive(dir, offline, r, hashErr == nil && digest == r.Checksum)
	if err != nil {
		return err
	}
	if source != "" {
		// 检出时若跳过 smudge，server/browser 里只剩一百多字节的指针。直接校验它会误报摘要不符。
		source, err = materializeBrowserArchive(ctx, source)
		if err != nil {
			return err
		}
		if err = copyBrowserArchive(source, archive); err != nil {
			return err
		}
	}
	digest, err = hashFile(archive)
	if err != nil {
		return err
	}
	if digest != r.Checksum {
		return fmt.Errorf("浏览器归档 SHA-256 不匹配，拒绝执行。请改用仓库 server/browser/ 中 Chrome for Testing %s 的官方离线 zip", browserVersion)
	}
	stage, err := os.MkdirTemp(root, ".browser-*")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, os.RemoveAll(stage)) }()
	manifest, err := extractZip(archive, stage)
	if err != nil {
		return err
	}
	destination := filepath.Join(root, "browser")
	if err = os.RemoveAll(destination); err != nil {
		return err
	}
	if err = os.Rename(stage, destination); err != nil {
		return err
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "browser-manifest.json"), encoded, 0600)
}

func containedPath(root, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("归档路径不能是绝对路径")
	}
	path := filepath.Join(root, filepath.FromSlash(name))
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("归档路径越界")
	}
	return path, nil
}

func checkResolvedPath(root, path string) error {
	// 临时目录和部署目录的父路径可能含系统符号链接，比较前统一到同一真实路径空间。
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(resolvedRoot, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("归档链接解析后越界")
	}
	return nil
}

func extractZip(archive, root string) (manifest map[string]string, result error) {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, reader.Close()) }()
	manifest = map[string]string{}
	var total int64
	// 先写普通文件，最后创建链接，避免链接父目录逃逸。
	links := map[string]string{}
	for _, entry := range reader.File {
		path, err := containedPath(root, entry.Name)
		if err != nil {
			return nil, err
		}
		if entry.FileInfo().IsDir() {
			if err = os.MkdirAll(path, 0700); err != nil {
				return nil, err
			}
			continue
		}
		if entry.UncompressedSize64 > uint64(maxExtractBytes) || total+int64(entry.UncompressedSize64) > maxExtractBytes {
			return nil, fmt.Errorf("归档展开超过大小上限")
		}
		total += int64(entry.UncompressedSize64)
		if _, exists := manifest[entry.Name]; exists {
			return nil, fmt.Errorf("归档包含重复路径")
		}
		source, err := entry.Open()
		if err != nil {
			return nil, err
		}
		if entry.Mode()&os.ModeSymlink != 0 {
			target, readErr := io.ReadAll(io.LimitReader(source, 4097))
			err = errors.Join(readErr, source.Close())
			if err != nil {
				return nil, err
			}
			if len(target) > 4096 {
				return nil, fmt.Errorf("归档链接过长")
			}
			if filepath.IsAbs(string(target)) {
				return nil, fmt.Errorf("归档链接越界")
			}
			relative := filepath.ToSlash(filepath.Join(filepath.Dir(entry.Name), string(target)))
			if _, err = containedPath(root, relative); err != nil {
				return nil, err
			}
			links[path] = string(target)
			manifest[entry.Name] = "link:" + string(target)
			continue
		}
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, errors.Join(err, source.Close())
		}
		dest, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, entry.Mode().Perm()&0777)
		if err != nil {
			return nil, errors.Join(err, source.Close())
		}
		_, copyErr := io.Copy(dest, source)
		if err = errors.Join(copyErr, dest.Close(), source.Close()); err != nil {
			return nil, err
		}
		digest, err := hashFile(path)
		if err != nil {
			return nil, err
		}
		manifest[entry.Name] = digest
	}
	for path, target := range links {
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		if err = os.Symlink(target, path); err != nil {
			return nil, err
		}
	}
	for path := range links {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, err
		}
		if err = checkResolvedPath(root, resolved); err != nil {
			return nil, err
		}
	}
	return manifest, nil
}
