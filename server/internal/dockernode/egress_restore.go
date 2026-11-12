package dockernode

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/oklog/ulid/v2"
)

// restoreEgress 在开放节点前逐批恢复端点；不恢复旧执行租约、不重建容器，也不将监听成功当作电脑就绪。
// 调用方持有节点生命周期及执行服务锁，并已清理全部旧执行；失败时必须关闭已恢复端点并终止启动。
func (node *Runtime) restoreEgress(ctx context.Context) (result error) {
	directory, err := node.journal.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, directory.Close()) }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(100)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, entry := range entries {
			name := entry.Name()
			if name == runtimeRecordName || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".session.json") || strings.HasSuffix(name, ".probe.json") || strings.HasSuffix(name, ".browser.json") {
				continue
			}
			userID := strings.TrimSuffix(name, ".json")
			if _, err := ulid.ParseStrict(userID); err != nil {
				return failure("egress_journal_name_invalid")
			}
			if err := node.restoreUserEgress(ctx, userID); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

func (node *Runtime) restoreUserEgress(ctx context.Context, userID string) (result error) {
	release, err := node.journal.lock(ctx, userID)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, release()) }()
	state, err := node.journal.read(userID)
	if err != nil {
		return err
	}
	if state == nil || state.Specification.NodeID != node.options.NodeID {
		return failure("egress_journal_identity_invalid")
	}
	resource := node.resource(state.Specification)
	current, err := node.load(ctx, resource, state)
	if err != nil {
		return err
	}
	if state.Phase == "absent" && !current.absent() {
		return failure("closed_generation_has_resources")
	}
	if current.hasContainer && current.container.State.Running && state.Phase != "removing" && state.Phase != "absent" {
		return node.egress.ensure(ctx, resource, current.container)
	}
	return node.egress.stop(ctx, resource)
}
