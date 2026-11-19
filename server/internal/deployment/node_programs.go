package deployment

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"io.yueyang/tom/internal/noderelease"
)

// programVersions 分开记录必须随容器重启的三个程序及每次会话重新启动的 Bot。
type programVersions struct {
	Core string `json:"core"`
	Bot  string `json:"bot"`
}

func (p programVersions) valid() bool {
	return noderelease.ValidDigest(p.Core) && noderelease.ValidDigest(p.Bot)
}

// stagePrograms 核对完整清单、宿主程序和四个 ELF，独占安装到内容寻址目录；已有进程始终能执行同代辅助程序。
func (i nodeInstaller) stagePrograms(ctx context.Context, directory string, node []byte) (programVersions, string, error) {
	var versions programVersions
	read := func(name string, limit int64) ([]byte, error) {
		return i.fs.readInstallationSource(filepath.Join(directory, name), limit, i.caller)
	}
	sums, err := read(noderelease.ProgramSums, 64<<10)
	if err != nil {
		return versions, "", err
	}
	files, err := noderelease.ParseChecksums(sums)
	if err != nil || len(files) != len(noderelease.ComputerPrograms)+2 || files["tomnode"] != contentDigest(node) || noderelease.ValidateExecutable(node, i.platform) != nil {
		return versions, "", errors.New("节点程序包不完整或 tomnode 身份不符")
	}
	raw, err := read("program-release.json", 64<<10)
	var profile noderelease.ProgramProfile
	if err != nil || contentDigest(raw) != files["program-release.json"] || decodePreparation(raw, &profile) != nil || !profile.Valid(i.platform) {
		return versions, "", errors.New("节点程序发布版本或平台无效")
	}
	versions.Bot = files["bot-agent"]
	var core strings.Builder
	for _, name := range noderelease.ComputerPrograms {
		if !noderelease.ValidDigest(files[name]) {
			return versions, "", errors.New("缺少电脑程序摘要：" + name)
		}
		if name != "bot-agent" {
			core.WriteString(name + " " + files[name] + "\n")
		}
	}
	versions.Core = contentDigest([]byte(core.String()))
	if err := i.fs.ensureInstallDirectory(filepath.Dir(noderelease.ProgramRoot), 0755); err != nil {
		return versions, "", err
	}
	for _, dir := range []string{noderelease.ProgramRoot, filepath.Join(noderelease.ProgramRoot, versions.Core), filepath.Join(noderelease.ProgramRoot, versions.Bot)} {
		if err := i.fs.prepareProgramDirectory(dir); err != nil {
			return versions, "", err
		}
	}
	for _, name := range noderelease.ComputerPrograms {
		bytes, err := read(name, maximumNodeBinaryBytes)
		if err != nil || contentDigest(bytes) != files[name] || noderelease.ValidateExecutable(bytes, i.platform) != nil {
			return versions, "", errors.New("电脑程序摘要或架构不符：" + name)
		}
		revision := versions.Core
		if name == "bot-agent" {
			revision = versions.Bot
		}
		dest := filepath.Join(noderelease.ProgramRoot, revision)
		if err := i.fs.installExactFile(ctx, filepath.Join(dest, name), bytes, 0755); err != nil {
			return versions, "", err
		}
	}
	return versions, contentDigest(sums), nil
}

// prepareProgramDirectory 显式设定新目录的遍历权限，systemd 更新器的 0077 掩码不能阻断容器内的非 root 程序。
func (fs filesystem) prepareProgramDirectory(dir string) error {
	path := fs.resolve(dir)
	if err := os.Mkdir(path, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := fs.directory(dir, account{}, false, false, false); err != nil {
		return err
	}
	if err := os.Chmod(path, 0755); err != nil {
		return err
	}
	return errors.Join(syncDirectory(path), syncDirectory(filepath.Dir(path)))
}

// verifyPrograms 复核安装记录选择的版本，发现旧目录或二进制损坏时阻止新会话使用它们。
func (fs filesystem) verifyPrograms(ctx context.Context, versions programVersions) error {
	if !versions.valid() {
		return errors.New("安装记录的程序版本无效")
	}
	var core strings.Builder
	for _, name := range noderelease.ComputerPrograms {
		if err := ctx.Err(); err != nil {
			return err
		}
		revision := versions.Core
		if name == "bot-agent" {
			revision = versions.Bot
		}
		rule := fileRule{path: filepath.Join(noderelease.ProgramRoot, revision, name), maximum: maximumNodeBinaryBytes}
		info, err := fs.inspect(rule, account{})
		if err != nil || info.Mode().Perm() != 0755 {
			return errors.New("已安装电脑程序的类型或权限无效：" + name)
		}
		content, err := fs.readInspectedFile(rule, info)
		if err != nil {
			return err
		}
		digest := contentDigest(content)
		if name == "bot-agent" {
			if digest != revision {
				return errors.New("已安装 Bot 程序摘要不符")
			}
		} else {
			core.WriteString(name + " " + digest + "\n")
		}
	}
	if contentDigest([]byte(core.String())) != versions.Core {
		return errors.New("已安装电脑基础程序摘要不符")
	}
	return nil
}

// activateBot 在受保护的父目录中原子切换入口。已启动的会话继续使用原可执行文件。
func (fs filesystem) activateBot(p programVersions) (failure error) {
	if !p.valid() {
		return errors.New("节点程序版本无效")
	}
	if err := fs.directory(noderelease.ProgramRoot, account{}, false, false, false); err != nil {
		return err
	}
	root := fs.resolve(noderelease.ProgramRoot)
	dst := filepath.Join(root, "bot")
	old, err := os.Readlink(dst)
	if err == nil && old == p.Bot {
		return syncDirectory(root)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && !noderelease.ValidDigest(old) {
		return errors.New("Bot 程序入口不是受管版本")
	}
	stage, err := os.MkdirTemp(root, ".activate-")
	if err != nil {
		return err
	}
	defer func() { failure = errors.Join(failure, os.RemoveAll(stage)) }()
	link := filepath.Join(stage, "bot")
	if err = os.Symlink(p.Bot, link); err != nil {
		return err
	}
	if err = os.Rename(link, dst); err != nil {
		return err
	}
	return syncDirectory(root)
}

// hasReleaseManifest 要求版本描述和摘要清单成对出现，避免将没有解压完整的包当作另一种格式。
func (fs filesystem) hasReleaseManifest(dir, profileName, sumsName string) (bool, error) {
	profile, err := fs.hasPackageFile(dir, profileName)
	if err != nil {
		return false, err
	}
	sums, err := fs.hasPackageFile(dir, sumsName)
	if err != nil {
		return false, err
	}
	if profile != sums {
		return false, errors.New("发布目录不完整，请从同一份归档解压 " + profileName + " 和 " + sumsName)
	}
	return profile, nil
}

// hasPackageFile 区分确实缺少资料和无法安全读取，调用者随后核对每个实际文件。
func (fs filesystem) hasPackageFile(dir, name string) (bool, error) {
	_, err := os.Lstat(fs.resolve(filepath.Join(dir, name)))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
