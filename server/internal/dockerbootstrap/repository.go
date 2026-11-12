package dockerbootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func (installation *installer) repository(ctx context.Context) error {
	sources, err := installation.host.directory("etc/apt/sources.list.d", 0755)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(sources)
	if err != nil {
		return err
	}
	if len(entries) > 200 {
		return errors.New("系统软件源数量超过引导检查上限")
	}
	paths := []string{installation.host.path("etc/apt/sources.list")}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".list") || strings.HasSuffix(entry.Name(), ".sources") {
			paths = append(paths, filepath.Join(sources, entry.Name()))
		}
	}
	source := repositorySource(installation.host.architecture)
	for _, path := range paths {
		contents, err := installation.host.readProtected(path, 1<<20)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if filepath.Base(path) == "tombot-docker.sources" {
			if string(contents) != source {
				return errors.New("已存在不同的 TomBot Docker 软件源配置，禁止自动覆盖")
			}
		} else if strings.Contains(string(contents), "download.docker.com") {
			return errors.New("已存在其他 Docker 软件源配置，禁止产生冲突来源")
		}
	}
	if err = installation.step("installing_dependencies"); err != nil {
		return err
	}
	if _, err = installation.apt(ctx, "update"); err != nil {
		return err
	}
	if _, err = installation.apt(ctx, "install", "--yes", "--no-remove", "--no-install-recommends", "ca-certificates", "gnupg"); err != nil {
		return err
	}
	if err = installation.step("verifying_repository"); err != nil {
		return err
	}
	key, err := installation.host.fetch(ctx)
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp(installation.stateDirectory, "repository-")
	if err != nil {
		return err
	}
	return installation.publishRepository(ctx, work, key, sources, source)
}

func (installation *installer) publishRepository(ctx context.Context, work string, key []byte, sources, source string) (result error) {
	defer func() { result = errors.Join(result, os.RemoveAll(work)) }()
	if len(key) == 0 || len(key) > 64*1024 {
		return errors.New("Docker 仓库签名内容大小无效")
	}
	if err := os.WriteFile(filepath.Join(work, "repository.asc"), key, 0600); err != nil {
		return err
	}
	output, err := installation.command(ctx, "usr/bin/gpg", "--batch", "--no-options", "--homedir", work, "--with-colons", "--fingerprint", "--show-keys", filepath.Join(work, "repository.asc"))
	if err != nil {
		return err
	}
	if err = verifyFingerprint(output, installation.manifest.RepositoryFingerprint); err != nil {
		return err
	}
	keyrings, err := installation.host.directory("etc/apt/keyrings", 0755)
	if err != nil {
		return err
	}
	if err = writeAtomic(keyrings, "tombot-docker.asc", key, 0644); err != nil {
		return err
	}
	if err = writeAtomic(sources, "tombot-docker.sources", []byte(source), 0644); err != nil {
		return err
	}
	if _, err = installation.apt(ctx, "update"); err != nil {
		return err
	}
	return nil
}

func verifyFingerprint(encoded []byte, expected string) error {
	primary := false
	count := 0
	verified := 0
	for _, line := range strings.Split(string(encoded), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 1 {
			continue
		}
		switch fields[0] {
		case "pub":
			if len(fields) < 2 || fields[1] == "r" || fields[1] == "e" || fields[1] == "i" || fields[1] == "d" {
				return errors.New("Docker 仓库签名已失效、撤销或禁用")
			}
			primary = true
			count++
		case "sub":
			if primary {
				return errors.New("Docker 仓库签名缺少主指纹")
			}
			primary = false
		case "fpr":
			if primary {
				if len(fields) < 10 || fields[9] != expected {
					return errors.New("Docker 仓库签名指纹与发布清单不符")
				}
				primary = false
				verified++
			}
		}
	}
	if count != 1 || verified != 1 || primary {
		return errors.New("Docker 仓库签名缺少唯一的有效主指纹")
	}
	return nil
}
