package dockernode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/infra/ids"
)

func advanceSpecification(specification computer.Specification) computer.Specification {
	specification.Generation++
	specification.Attempt++
	specification.FencingToken++
	return specification
}

func closeRetirementFixture(t *testing.T) (fixture, Resource) {
	t.Helper()
	f := newFixture(t)
	if _, err := f.node.Ensure(t.Context(), f.specification); err != nil {
		t.Fatal(err)
	}
	if _, err := f.node.Remove(t.Context(), f.specification); err != nil {
		t.Fatal(err)
	}
	return f, f.node.resource(f.specification)
}

func TestAgentRetirementBoundsRepeatedGenerations(t *testing.T) {
	f := newFixture(t)
	specification := f.specification
	sharedFile := filepath.Join(f.node.options.StorageDirectory, "shared-document")
	if err := os.WriteFile(sharedFile, []byte("共享业务内容"), 0600); err != nil {
		t.Fatal(err)
	}
	var previous string
	for range 24 {
		if _, err := f.node.Ensure(t.Context(), specification); err != nil {
			t.Fatal(err)
		}
		if previous != "" {
			if _, err := os.Lstat(previous); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("新代次保留了旧控制目录", err)
			}
		}
		resource := f.node.resource(specification)
		for _, name := range []string{"agent.lock", computeragent.EgressSocketName + ".lock", "probe.json", ids.New() + ".session.json", ".session-" + ids.New() + ".tmp", ".write-" + ids.New()} {
			if err := os.WriteFile(filepath.Join(resource.ControlPath, name), []byte("关闭记录"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.node.Remove(t.Context(), specification); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(resource.ControlPath, "probe.json")); err != nil {
			t.Fatal("当前最高代次的关闭记录被提前删除", err)
		}
		entries, err := os.ReadDir(f.statePath)
		if err != nil || len(entries) != 3 {
			t.Fatal("周期验收造成控制目录累积", len(entries), err)
		}
		previous = resource.ControlPath
		specification = advanceSpecification(specification)
	}
	actual, err := os.ReadFile(sharedFile)
	if err != nil || string(actual) != "共享业务内容" {
		t.Fatal("控制元数据回收影响共享文件", err)
	}
}

func TestAgentRetirementKeepsHostSessionBarrier(t *testing.T) {
	f := newFixture(t)
	session := f.prepareComputer(t, f.specification)
	state := &sessionRecord{Version: 2, Command: commandFor(session), Epoch: ids.New(), Phase: "closed", Deadline: time.Now().UTC()}
	if err := f.node.journal.saveSession(state); err != nil {
		t.Fatal(err)
	}
	name := sessionKeyOf(session).name() + ".json"
	before, err := f.node.journal.readFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.node.Remove(t.Context(), f.specification); err != nil {
		t.Fatal(err)
	}
	if _, err := f.node.Inspect(t.Context(), advanceSpecification(f.specification)); err != nil {
		t.Fatal(err)
	}
	after, err := f.node.journal.readFile(name)
	if err != nil || string(after) != string(before) {
		t.Fatal("回收旧代理目录删除或重写了宿主消费屏障", err)
	}
}

func TestAgentRetirementRejectsUnsafeResources(t *testing.T) {
	for _, scenario := range []string{"未知文件", "子目录", "符号链接", "硬链接", "权限", "配置身份", "目录挂载", "文件挂载", "挂载读取失败", "代理占用", "出网占用", "探针未关闭", "出网未关闭", "物理资源残留"} {
		t.Run(scenario, func(t *testing.T) {
			f, resource := closeRetirementFixture(t)
			configuration := filepath.Join(resource.ControlPath, computeragent.ConfigName)
			before, err := os.ReadFile(configuration)
			if err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "preserved")
			if err := os.WriteFile(outside, []byte("外部内容"), 0600); err != nil {
				t.Fatal(err)
			}
			var setupErr error
			switch scenario {
			case "未知文件":
				setupErr = os.WriteFile(filepath.Join(resource.ControlPath, "unknown"), []byte("保留"), 0600)
			case "子目录":
				setupErr = os.Mkdir(filepath.Join(resource.ControlPath, "nested"), 0700)
			case "符号链接":
				setupErr = os.Symlink(outside, filepath.Join(resource.ControlPath, "probe.json"))
			case "硬链接":
				setupErr = os.Link(outside, filepath.Join(resource.ControlPath, "probe.json"))
			case "权限":
				setupErr = os.Chmod(configuration, 0660)
			case "配置身份":
				config := f.node.agentConfiguration(resource)
				config.Identity.UserID = ids.New()
				before, setupErr = agentchannel.Encode(config)
				if setupErr == nil {
					setupErr = os.WriteFile(configuration, before, 0600)
				}
			case "目录挂载", "文件挂载":
				target := resource.ControlPath
				if scenario == "文件挂载" {
					target = configuration
				}
				f.node.mounts = func() ([]filesystemMount, error) { return []filesystemMount{{target: target}}, nil }
			case "挂载读取失败":
				f.node.mounts = func() ([]filesystemMount, error) { return nil, errors.New("测试挂载查询失败") }
			case "代理占用", "出网占用":
				name := "agent.lock"
				if scenario == "出网占用" {
					name = computeragent.EgressSocketName + ".lock"
				}
				file, err := os.OpenFile(filepath.Join(resource.ControlPath, name), os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := file.Close(); err != nil {
						t.Error(err)
					}
				})
				setupErr = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			case "探针未关闭":
				f.node.probes.(*fakeComputerProbes).retireError = errors.New("测试探针关闭失败")
			case "出网未关闭":
				f.node.egress.(*fakeEgress).stopCall = func(context.Context, Resource) error { return errors.New("测试出网关闭失败") }
			case "物理资源残留":
				f.storage.workspaces[resource.Name] = Workspace{Exists: true, Path: resource.WorkspacePath, CapacityBytes: resource.Specification.Resources.DiskBytes}
			}
			if setupErr != nil {
				t.Fatal(setupErr)
			}
			if _, err := f.node.Ensure(t.Context(), advanceSpecification(f.specification)); err == nil {
				t.Fatal("危险控制目录未阻止下一代")
			}
			state, err := f.node.journal.read(f.specification.UserID)
			if err != nil || state.Specification.Generation != f.specification.Generation || state.Retirement != nil || f.engine.creates != 1 {
				t.Fatal("失败回收推进了代次或建立新容器", err)
			}
			actual, err := os.ReadFile(configuration)
			if err != nil || string(actual) != string(before) {
				t.Fatal("校验未完成就删除控制文件", err)
			}
			actual, err = os.ReadFile(outside)
			if err != nil || string(actual) != "外部内容" {
				t.Fatal("回收改变外部文件", err)
			}
		})
	}
}

func TestAgentRetirementRequiresClosedHostSessions(t *testing.T) {
	f := newFixture(t)
	session := f.prepareComputer(t, f.specification)
	if _, err := f.node.Remove(t.Context(), f.specification); err != nil {
		t.Fatal(err)
	}
	state := &sessionRecord{Version: 2, Command: commandFor(session), Epoch: ids.New(), Phase: "closing", Deadline: time.Now().UTC()}
	if err := f.node.journal.saveSession(state); err != nil {
		t.Fatal(err)
	}
	if _, err := f.node.Ensure(t.Context(), advanceSpecification(f.specification)); err == nil {
		t.Fatal("执行会话未关闭仍回收控制目录")
	}
	if _, err := os.Stat(f.node.resource(f.specification).ControlPath); err != nil {
		t.Fatal("未关闭会话的控制目录丢失", err)
	}
}

func TestAgentRetirementResumesAfterInterruption(t *testing.T) {
	for _, scenario := range []string{"空目录", "目录已删除", "目录被替换"} {
		t.Run(scenario, func(t *testing.T) {
			f, resource := closeRetirementFixture(t)
			next := advanceSpecification(f.specification)
			next.FencingToken += 2
			calls := 0
			f.node.mounts = func() ([]filesystemMount, error) {
				calls++
				if calls == 2 {
					return nil, errors.New("测试删除完成后中断")
				}
				return nil, nil
			}
			if _, err := f.node.Ensure(t.Context(), next); err == nil {
				t.Fatal("未报告中断")
			}
			state, err := f.node.journal.read(f.specification.UserID)
			if err != nil || state.Retirement == nil || state.Specification.Generation != f.specification.Generation || state.Phase != "absent" {
				t.Fatal("删除前未持久保存目录身份和最高代次", err)
			}
			if _, err := f.node.Ensure(t.Context(), f.specification); err == nil {
				t.Fatal("旧代次在删除中途恢复创建")
			}
			for _, scenario := range []string{"旧控制者", "改资源", "复用令牌改期限"} {
				stale := next
				switch scenario {
				case "旧控制者":
					stale.FencingToken--
				case "改资源":
					stale.FencingToken++
					stale.Resources.CPUMillis++
				case "复用令牌改期限":
					stale.LeaseExpiresAt = stale.LeaseExpiresAt.Add(time.Second)
				}
				_, err := f.node.Ensure(t.Context(), stale)
				requireFailure(t, err, "stale_controller")
			}
			entries, err := os.ReadDir(resource.ControlPath)
			if err != nil || len(entries) != 0 {
				t.Fatal("测试未到达配置已删除的中断边界", err)
			}
			if scenario == "目录已删除" {
				if err := os.Remove(resource.ControlPath); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "目录被替换" {
				if err := os.Rename(resource.ControlPath, resource.ControlPath+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(resource.ControlPath, 0700); err != nil {
					t.Fatal(err)
				}
			}
			journal, err := openJournal(f.statePath, uint32(os.Geteuid()))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := journal.root.Close(); err != nil {
					t.Error(err)
				}
			}()
			restarted := *f.node
			restarted.journal = journal
			next.FencingToken++
			next.LeaseExpiresAt = next.LeaseExpiresAt.Add(time.Second)
			_, err = restarted.Ensure(t.Context(), next)
			if scenario == "目录被替换" {
				requireFailure(t, err, "agent_retirement_directory_replaced")
				if _, err := os.Stat(resource.ControlPath); err != nil {
					t.Fatal("替代目录被删除", err)
				}
				return
			}
			if err != nil {
				t.Fatal("重启未恢复部分回收", err)
			}
			if _, err := os.Stat(resource.ControlPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("旧目录仍存在", err)
			}
			state, err = journal.read(next.UserID)
			if err != nil || state.Retirement != nil || state.Specification.Generation != next.Generation {
				t.Fatal("新代次提交不完整", err)
			}
		})
	}
}

func TestAgentRetirementRejectsInvalidPersistentBarrier(t *testing.T) {
	for _, scenario := range []string{"旧版本", "未关闭", "缺少目录身份", "跳代", "换用户", "无效规格", "旧控制者"} {
		t.Run(scenario, func(t *testing.T) {
			f, resource := closeRetirementFixture(t)
			state, err := f.node.journal.read(f.specification.UserID)
			if err != nil {
				t.Fatal(err)
			}
			stat, err := os.Stat(resource.ControlPath)
			if err != nil {
				t.Fatal(err)
			}
			identity := stat.Sys().(*syscall.Stat_t)
			state.Retirement = &controlRetirement{Device: uint64(identity.Dev), Inode: identity.Ino, Next: advanceSpecification(f.specification)}
			switch scenario {
			case "旧版本":
				state.Version--
			case "未关闭":
				state.Phase = "ready"
			case "缺少目录身份":
				state.Retirement.Inode = 0
			case "跳代":
				state.Retirement.Next.Generation++
			case "换用户":
				state.Retirement.Next.UserID = ids.New()
			case "无效规格":
				state.Retirement.Next.Resources.DiskBytes = 0
			case "旧控制者":
				state.Retirement.Next.FencingToken--
			}
			if err := f.node.journal.save(state); err != nil {
				t.Fatal(err)
			}
			if _, err := f.node.Inspect(t.Context(), advanceSpecification(f.specification)); err == nil {
				t.Fatal("损坏的回收记录被接受")
			}
			if _, err := os.Stat(filepath.Join(resource.ControlPath, computeragent.ConfigName)); err != nil {
				t.Fatal("损坏屏障导致控制目录被删除", err)
			}
		})
	}
}

func TestAgentRetirementConcurrentNextGeneration(t *testing.T) {
	f, resource := closeRetirementFixture(t)
	next := advanceSpecification(f.specification)
	var workers sync.WaitGroup
	results := make(chan error, 12)
	for range cap(results) {
		workers.Go(func() {
			_, err := f.node.Ensure(t.Context(), next)
			results <- err
		})
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.engine.creates != 2 {
		t.Fatal("并发换代重复创建容器", f.engine.creates)
	}
	if _, err := os.Stat(resource.ControlPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("并发换代未回收旧目录", err)
	}
}

func TestAgentRetirementBeforeConfigurationExists(t *testing.T) {
	f := newFixture(t)
	if _, err := f.node.Remove(t.Context(), f.specification); err != nil {
		t.Fatal(err)
	}
	resource := f.node.resource(f.specification)
	if err := os.Mkdir(resource.ControlPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resource.ControlPath, ".write-"+ids.New()), []byte("未提交配置"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.node.Ensure(t.Context(), advanceSpecification(f.specification)); err != nil {
		t.Fatal("配置创建前中断不能恢复", err)
	}
	if _, err := os.Stat(resource.ControlPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("空代次目录没有回收", err)
	}
}

func TestAgentRetirementEntryBound(t *testing.T) {
	f, resource := closeRetirementFixture(t)
	for range computer.MaxDesktops + 64 {
		if err := os.WriteFile(filepath.Join(resource.ControlPath, ".session-"+ids.New()+".tmp"), []byte("未提交记录"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := f.node.Ensure(t.Context(), advanceSpecification(f.specification))
	requireFailure(t, err, "agent_retirement_entry_limit")
	entries, err := os.ReadDir(resource.ControlPath)
	if err != nil || len(entries) != computer.MaxDesktops+65 {
		t.Fatal("超出上限时已经删除了控制内容", len(entries), err)
	}
}

func TestNodeEmptyGenerationsCloseStorageBarrier(t *testing.T) {
	f := newFixture(t)
	storage := newStorageFixture(t).storage
	storage.options.Directory = f.node.options.StorageDirectory
	f.node.storage = storage
	specification := f.specification
	for range 12 {
		if _, err := f.node.Remove(t.Context(), specification); err != nil {
			t.Fatal(err)
		}
		state, err := storage.load(f.node.resource(specification))
		if err != nil || state == nil || state.Phase != "removed" || state.Generation != specification.Generation {
			t.Fatal("节点只关闭物理存在的存储代次", err)
		}
		specification = advanceSpecification(specification)
	}
	entries, err := os.ReadDir(storage.options.StateDirectory)
	if err != nil || len(entries) != 2 {
		t.Fatal("空代次增长了磁盘记录", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), specification.UserID+".storage.") {
			t.Fatal("非稳定存储记录", entry.Name())
		}
	}
}
