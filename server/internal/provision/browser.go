package provision

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const browserVersion = "157.0.8103.38"
const maxArchiveBytes int64 = 512 << 20
const maxExtractBytes int64 = 2 << 30

type release struct{ Platform, Checksum, Executable string }

func (r release) archiveURL() string {
	return "https://storage.googleapis.com/chrome-for-testing-public/" + browserVersion + "/" + r.Platform + "/chrome-" + r.Platform + ".zip"
}

// 固定资产由官方 HTTPS 下载后计算 SHA-256；升级必须同时更新版本与校验值。
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

// EnsureBrowser 校验来源后在临时目录展开，再替换运行目录；离线资产也必须匹配固定摘要。
func EnsureBrowser(ctx context.Context, dir, offline string) (result error) {
	r, err := currentRelease()
	if err != nil {
		return err
	}
	root := filepath.Join(dir, "runtime")
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	archive := filepath.Join(root, "chrome-"+browserVersion+"-"+r.Platform+".zip")
	if offline != "" {
		source, err := os.Open(offline)
		if err != nil {
			return err
		}
		dest, err := os.OpenFile(archive, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return errors.Join(err, source.Close())
		}
		written, copyErr := io.Copy(dest, io.LimitReader(source, maxArchiveBytes+1))
		err = errors.Join(copyErr, dest.Close(), source.Close())
		if err != nil {
			return err
		}
		if written > maxArchiveBytes {
			return fmt.Errorf("浏览器归档超过大小上限")
		}
	} else {
		digest, hashErr := hashFile(archive)
		if hashErr != nil || digest != r.Checksum {
			if err = download(ctx, r.archiveURL(), archive); err != nil {
				return fmt.Errorf("下载浏览器失败；可设置 TOM_BROWSER_ARCHIVE 使用官方离线 zip：%w", err)
			}
		}
	}
	digest, err := hashFile(archive)
	if err != nil {
		return err
	}
	if digest != r.Checksum {
		return fmt.Errorf("浏览器归档 SHA-256 不匹配，拒绝执行")
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

func download(ctx context.Context, address, path string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(time.Second * time.Duration(1<<uint(attempt-1)))
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		last = downloadAttempt(ctx, client, address, path)
		if last == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return last
}

func downloadAttempt(ctx context.Context, client *http.Client, address, path string) error {
	partial := path + ".part"
	var offset int64
	if stat, err := os.Stat(partial); err == nil {
		offset = stat.Size()
	} else if !os.IsNotExist(err) {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	if offset > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("资产服务返回状态 %d", response.StatusCode)
	}
	flags := os.O_CREATE | os.O_WRONLY
	if response.StatusCode == http.StatusPartialContent {
		prefix := "bytes " + strconv.FormatInt(offset, 10) + "-"
		if !strings.HasPrefix(response.Header.Get("Content-Range"), prefix) {
			return fmt.Errorf("下载续传区间无效")
		}
		flags |= os.O_APPEND
	} else {
		offset = 0
		flags |= os.O_TRUNC
	}
	if offset > maxArchiveBytes {
		return fmt.Errorf("下载文件超过大小上限")
	}
	file, err := os.OpenFile(partial, flags, 0600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, maxArchiveBytes-offset+1))
	err = errors.Join(copyErr, file.Close())
	if err != nil {
		return err
	}
	if written+offset > maxArchiveBytes {
		return fmt.Errorf("下载文件超过大小上限")
	}
	return os.Rename(partial, path)
}
