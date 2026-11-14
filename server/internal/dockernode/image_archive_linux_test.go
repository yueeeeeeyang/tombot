//go:build linux

package dockernode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type archiveEngine struct {
	*fakeImageEngine
	loads     int
	content   []byte
	loadError error
}

func (engine *archiveEngine) loadImage(_ context.Context, reader io.Reader) error {
	engine.loads++
	content, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	if !bytes.Equal(content, engine.content) {
		return errors.New("归档内容不符")
	}
	if engine.loadError != nil {
		return engine.loadError
	}
	engine.exists = true
	return nil
}

// TestOfflineImageArchive 在 Linux root 临时目录验证实际文件边界，不访问仓库或启动业务容器。
func TestOfflineImageArchive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("离线归档私有文件检查要求 Linux root，另在受限容器验收")
	}
	for _, kind := range []string{"正常", "摘要错误", "权限过宽", "符号链接", "导入失败"} {
		t.Run(kind, func(t *testing.T) {
			directory, err := os.MkdirTemp("/var/lib", "tombot-image-test-")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.RemoveAll(directory); err != nil {
					t.Error(err)
				}
			}()
			preparer, manifest, engine, _ := imagePreparerFixture(t)
			content := []byte("离线镜像导入夹具")
			digest := sha256.Sum256(content)
			archive := &archiveEngine{fakeImageEngine: engine, content: content}
			preparer.engine = archive
			manifest.ArchiveFile = filepath.Join(directory, "computer-image.tar")
			manifest.ArchiveSHA256 = hex.EncodeToString(digest[:])
			if err := os.WriteFile(manifest.ArchiveFile, content, 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "摘要错误":
				manifest.ArchiveSHA256 = strings.Repeat("a", 64)
			case "权限过宽":
				if err := os.Chmod(manifest.ArchiveFile, 0644); err != nil {
					t.Fatal(err)
				}
			case "符号链接":
				alias := filepath.Join(directory, "alias.tar")
				if err := os.Symlink(manifest.ArchiveFile, alias); err != nil {
					t.Fatal(err)
				}
				manifest.ArchiveFile = alias
			case "导入失败":
				archive.loadError = errors.New("导入未确认")
			}
			report, err := preparer.prepare(t.Context(), manifest)
			if kind == "正常" {
				if err != nil || report.State != "available" || archive.loads != 1 || engine.pulls != 0 {
					t.Fatal("没有按离线归档准备", err)
				}
				if _, err := preparer.prepare(t.Context(), manifest); err != nil || archive.loads != 1 {
					t.Fatal("重试重复导入已核实镜像", err)
				}
			} else {
				if err == nil || report.State != "" || engine.pulls != 0 {
					t.Fatal("失败被掩盖或回退联网拉取")
				}
				if kind != "导入失败" && archive.loads != 0 {
					t.Fatal("文件校验失败仍导入")
				}
			}
		})
	}
}
