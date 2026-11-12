//go:build dockerintegration

package dockernode

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// readOnlyImageEngine 仅允许真实引擎查询；测试失败也不会意外下载或修改镜像。
type readOnlyImageEngine struct{ engine *dockerEngine }

func (engine readOnlyImageEngine) inspectImage(ctx context.Context, reference string) (imageDescription, error) {
	return engine.engine.inspectImage(ctx, reference)
}
func (readOnlyImageEngine) pullImage(context.Context, string, string, string) error {
	return errors.New("只读验收禁止拉取镜像")
}

// TestLiveImagePreparation 核验已有正式镜像的实际 API 元数据及重复准备，不启动、推送或删除镜像。
func TestLiveImagePreparation(t *testing.T) {
	socket, reference := os.Getenv("TOM_TEST_DOCKER_SOCKET"), os.Getenv("TOM_TEST_COMPUTER_IMAGE")
	if socket == "" || reference == "" {
		t.Fatal("必须显式提供本机 Docker socket 和已有电脑镜像固定摘要")
	}
	preparer, manifest, _, _ := imagePreparerFixture(t)
	manifest.Reference = reference
	if err := manifest.validate(); err != nil {
		t.Fatal(err)
	}
	engine, err := newEngine(socket, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	actual, err := engine.inspectImage(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	if err := actual.validate(reference, manifest.Platform); err != nil {
		t.Fatal("实际镜像摘要、平台或固定入口不符", err)
	}
	preparer.engine = readOnlyImageEngine{engine}
	for range 2 {
		report, err := preparer.prepare(ctx, manifest)
		if err != nil || report.DockerImageID != actual.ID || report.Reference != reference || report.State != "available" {
			t.Fatal(report, err)
		}
	}
	t.Log("实际固定镜像及重复准备已通过，只读检查没有调用拉取接口")
}
