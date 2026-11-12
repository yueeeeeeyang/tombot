package dockernode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"io.yueyang/tom/internal/infra/ids"
)

func nextStorageGeneration(fixture *storageFixture) {
	fixture.resource.Specification.Generation++
	fixture.resource.Specification.Attempt++
	fixture.resource.Specification.FencingToken++
	fixture.resource.Name = resourceName(fixture.resource.Specification)
	fixture.resource.WorkspacePath = filepath.Join(fixture.storage.options.Directory, fixture.resource.Name, "workspace")
}

func TestStorageRetentionAcrossPhysicalGenerations(t *testing.T) {
	fixture := newStorageFixture(t)
	first := fixture.resource
	for range 12 {
		if _, err := fixture.storage.ensure(t.Context(), fixture.resource); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.storage.remove(t.Context(), fixture.resource); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(fixture.storage.options.StateDirectory)
		if err != nil || len(entries) != 2 { // 最高代次记录与节点 loop 分配锁。
			t.Fatal("周期供给导致控制元数据增长", len(entries), err)
		}
		if err := fixture.storage.validateStorageLayout(); err != nil {
			t.Fatal(err)
		}
		nextStorageGeneration(fixture)
	}
	if fixture.formats != 12 {
		t.Fatal("独立代次未各自建立文件系统", fixture.formats)
	}
	if _, err := fixture.storage.ensure(t.Context(), first); err == nil {
		t.Fatal("旧代次重新创建了磁盘")
	}
	entries, err := os.ReadDir(fixture.storage.options.Directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("旧资源目录未关闭", err)
	}
}

func TestStorageEmptyGenerationsRetainOneStableBarrier(t *testing.T) {
	fixture := newStorageFixture(t)
	fixture.resource.Specification.Resources.DiskBytes = 512 << 20
	first := fixture.resource
	var lockIdentity os.FileInfo
	for range 24 {
		if err := fixture.storage.Remove(t.Context(), fixture.resource); err != nil {
			t.Fatal(err)
		}
		if err := fixture.storage.Remove(t.Context(), fixture.resource); err != nil {
			t.Fatal("重复关闭失败", err)
		}
		if _, err := fixture.storage.Ensure(t.Context(), fixture.resource); err == nil {
			t.Fatal("空代次关闭后被重新创建")
		}
		entries, err := os.ReadDir(fixture.storage.options.StateDirectory)
		if err != nil || len(entries) != 2 {
			t.Fatal("空代次增加了元数据文件", len(entries), err)
		}
		stat, err := os.Stat(filepath.Join(fixture.storage.options.StateDirectory, first.Specification.UserID+".storage.lock"))
		if err != nil || lockIdentity != nil && !os.SameFile(lockIdentity, stat) {
			t.Fatal("跨代次替换了稳定锁", err)
		}
		lockIdentity = stat
		nextStorageGeneration(fixture)
	}
	if len(fixture.commands) != 0 {
		t.Fatal("空代次触发了内核磁盘操作", fixture.commands)
	}
	_, err := fixture.storage.Inspect(t.Context(), first)
	requireFailure(t, err, "storage_generation_not_released")
	// 用新目录句柄模拟进程重启，最高代次仍由持久记录决定。
	journal, err := openJournal(fixture.storage.options.StateDirectory, fixture.storage.owner)
	if err != nil {
		t.Fatal(err)
	}
	restarted := *fixture.storage
	restarted.journal = journal
	defer func() {
		if err := restarted.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := restarted.Remove(t.Context(), fixture.resource); err != nil {
		t.Fatal("重启后下一代无法关闭", err)
	}
}

func TestStorageGenerationsShareExclusiveLock(t *testing.T) {
	fixture := newStorageFixture(t)
	fixture.resource.Specification.Resources.DiskBytes = 512 << 20
	release, err := fixture.storage.journal.lock(t.Context(), fixture.resource.Specification.UserID+".storage")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		err = fixture.storage.Remove(ctx, fixture.resource)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("不同代次未争用同一把锁", err)
		}
		nextStorageGeneration(fixture)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if len(fixture.commands) != 0 {
		t.Fatal("未取得锁仍调用了宿主工具")
	}
}

func TestStorageRetentionRejectsUnsafeTransitions(t *testing.T) {
	for _, scenario := range []string{"未关闭", "跳代", "旧代", "身份", "同代容量", "旧目录", "版本", "状态", "未知字段"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newStorageFixture(t)
			state := initialStorageRecord(fixture.resource)
			state.Phase = "removed"
			if err := fixture.storage.save(state); err != nil {
				t.Fatal(err)
			}
			previous := fixture.resource
			nextStorageGeneration(fixture)
			switch scenario {
			case "未关闭":
				state.Phase = "ready"
			case "跳代":
				nextStorageGeneration(fixture)
			case "旧代":
				state = initialStorageRecord(fixture.resource)
				state.Phase = "removed"
				fixture.resource = previous
			case "身份":
				state.OperationID = ids.New()
			case "同代容量":
				fixture.resource = previous
				fixture.resource.Specification.Resources.DiskBytes *= 2
			case "旧目录":
				if err := os.Mkdir(fixture.storage.directory(previous), 0700); err != nil {
					t.Fatal(err)
				}
			case "版本":
				state.Version--
			case "状态":
				state.Phase = "unknown"
			}
			encoded, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "未知字段" {
				encoded = append(encoded[:len(encoded)-1], []byte(`,"other":true}`)...)
			}
			name := state.UserID + ".storage.json"
			if err := fixture.storage.journal.writeFile(name, encoded); err != nil {
				t.Fatal(err)
			}
			for _, action := range []func(context.Context, Resource) (Workspace, error){fixture.storage.inspect, fixture.storage.ensure, fixture.storage.remove} {
				if _, err := action(t.Context(), fixture.resource); err == nil {
					t.Fatal("危险代次变更未被拒绝")
				}
			}
			actual, err := fixture.storage.journal.readFile(name)
			if err != nil || string(actual) != string(encoded) || len(fixture.commands) != 0 {
				t.Fatal("失败请求改写了屏障或执行磁盘操作", err)
			}
		})
	}
}

func TestStorageRejectsLegacyOrUnknownJournalLayout(t *testing.T) {
	for _, suffix := range []string{"旧记录", "旧锁", "错误后缀", "未知文件"} {
		t.Run(suffix, func(t *testing.T) {
			fixture := newStorageFixture(t)
			name := map[string]string{"旧记录": fixture.resource.Name + ".json", "旧锁": fixture.resource.Name + ".lock", "错误后缀": fixture.resource.Specification.UserID + ".storage.lock.storage.json", "未知文件": "unknown"}[suffix]
			if err := fixture.storage.journal.writeFile(name, []byte("保留内容")); err != nil {
				t.Fatal(err)
			}
			requireFailure(t, fixture.storage.validateStorageLayout(), "storage_journal_layout_invalid")
			actual, err := fixture.storage.journal.readFile(name)
			if err != nil || string(actual) != "保留内容" {
				t.Fatal("拒绝旧布局时删除了记录", err)
			}
		})
	}
}
