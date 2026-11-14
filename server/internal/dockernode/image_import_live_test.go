//go:build dockerintegration

package dockernode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

// TestLiveOfflineImageImport 要求测试镜像尚未在引擎中存在，验证实际归档导入后的摘要和重复准备。
// 只导入调用者明确提供的本地归档，不删除镜像、不联系仓库或启动电脑。
func TestLiveOfflineImageImport(t *testing.T) {
	socket := os.Getenv("TOM_TEST_DOCKER_SOCKET")
	reference := os.Getenv("TOM_TEST_COMPUTER_IMAGE")
	archivePath := os.Getenv("TOM_TEST_IMAGE_ARCHIVE")
	checksum := os.Getenv("TOM_TEST_IMAGE_ARCHIVE_SHA256")
	platform := os.Getenv("TOM_TEST_IMAGE_PLATFORM")
	if socket == "" || reference == "" || archivePath == "" || checksum == "" || platform == "" {
		t.Fatal("必须显式提供本机引擎、镜像摘要、归档、归档摘要及平台")
	}
	preparer, manifest, _, _ := imagePreparerFixture(t)
	manifest.Reference, manifest.Platform = reference, platform
	if err := manifest.validate(); err != nil {
		t.Fatal(err)
	}
	engine, err := newEngine(socket, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	if _, err := engine.inspectImage(ctx, reference); !errors.Is(err, errMissing) {
		t.Fatal("此验收要求测试镜像尚未存在，或引擎返回了未知错误", err)
	}
	archive, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := archive.Close(); err != nil {
			t.Error(err)
		}
	}()
	hash := sha256.New()
	if _, err := io.Copy(hash, &contextReader{ctx: ctx, reader: archive}); err != nil || hex.EncodeToString(hash.Sum(nil)) != checksum {
		t.Fatal("归档摘要不符", err)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := engine.loadImage(ctx, archive); err != nil {
		t.Fatal(err)
	}
	preparer.engine = readOnlyImageEngine{engine}
	for range 2 {
		report, err := preparer.prepare(ctx, manifest)
		if err != nil || report.State != "available" || report.Reference != reference {
			t.Fatal("归档导入后固定摘要或镜像契约核验失败", err)
		}
	}
	t.Log("真实离线导入保留固定摘要和镜像契约，重复准备不联系仓库")
}
