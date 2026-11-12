package dockernode

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computeragent"
)

func TestAgentConfigurationRejectsPreBrowserSupervisorVersion(t *testing.T) {
	fixture := newFixture(t)
	resource := fixture.node.resource(fixture.specification)
	if _, err := fixture.node.Ensure(t.Context(), fixture.specification); err != nil {
		t.Fatal(err)
	}
	previous := fixture.node.agentConfiguration(resource)
	previous.Version = 3
	encoded, err := agentchannel.Encode(previous)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(resource.ControlPath, computeragent.ConfigName)
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	// 旧代理不理解浏览器关闭屏障，节点不能原地改写配置使其继续接单。
	for _, create := range []bool{false, true} {
		if err := fixture.node.configureAgent(resource, create); err == nil {
			t.Fatal("缺少浏览器监督的旧代理配置未拒绝")
		}
	}
	retained, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(retained, encoded) {
		t.Fatal("旧实例配置被覆盖", err)
	}
}

func TestAgentDirectoryIsFixedAndRetainsCloseBarrier(t *testing.T) {
	fixture := newFixture(t)
	resource := fixture.node.resource(fixture.specification)
	if _, err := fixture.node.Ensure(context.Background(), fixture.specification); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(filepath.Join(resource.ControlPath, computeragent.ConfigName))
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.node.configureAgent(resource, true); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(filepath.Join(resource.ControlPath, computeragent.ConfigName))
	if err != nil || string(current) != string(encoded) {
		t.Fatal("重复配置修改既有实例", err)
	}
	barrier := filepath.Join(resource.ControlPath, "session.json")
	if err = os.WriteFile(barrier, []byte("控制屏障保留验证"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.node.Remove(context.Background(), fixture.specification); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(barrier); err != nil {
		t.Fatal("资源删除错误地删除代理关闭记录", err)
	}
	next := fixture.specification
	next.Generation++
	next.Attempt++
	next.FencingToken++
	if fixture.node.resource(next).ControlPath == resource.ControlPath {
		t.Fatal("不同实例代次复用控制目录")
	}
}

func TestAgentConfigurationAndMountDriftRejected(t *testing.T) {
	for _, change := range []string{"配置", "目录权限", "目录链接", "挂载", "替换控制文件"} {
		t.Run(change, func(t *testing.T) {
			fixture := newFixture(t)
			resource := fixture.node.resource(fixture.specification)
			if _, err := fixture.node.Ensure(context.Background(), fixture.specification); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "配置":
				if err := os.WriteFile(filepath.Join(resource.ControlPath, computeragent.ConfigName), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "目录权限":
				if err := os.Chmod(resource.ControlPath, 0777); err != nil {
					t.Fatal(err)
				}
			case "目录链接":
				other := resource.ControlPath + "-original"
				if err := os.Rename(resource.ControlPath, other); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, resource.ControlPath); err != nil {
					t.Fatal(err)
				}
			case "挂载":
				fixture.engine.mu.Lock()
				value := fixture.engine.containers[resource.Name]
				for index := range value.Mounts {
					if value.Mounts[index].Destination == computeragent.ControlDirectory {
						value.Mounts[index].Source = fixture.node.options.StateDirectory
					}
				}
				fixture.engine.containers[resource.Name] = value
				fixture.engine.mu.Unlock()
			case "替换控制文件":
				name := filepath.Join(resource.ControlPath, computeragent.ConfigName)
				if err := os.Rename(name, name+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(name+"-original", name); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := fixture.node.Inspect(context.Background(), fixture.specification); err == nil {
				t.Fatal("实例漂移未阻止对账")
			}
			if _, err := fixture.node.Ensure(context.Background(), fixture.specification); err == nil {
				t.Fatal("实例漂移被自动覆盖")
			}
		})
	}
}
