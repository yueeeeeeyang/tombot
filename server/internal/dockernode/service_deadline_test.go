package dockernode

import (
	"context"
	"errors"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/processgroup"
)

func TestServiceDeadlineBindsVerifiedProcess(t *testing.T) {
	for _, mode := range []string{"正常换算", "时钟失败", "进程替换", "容器停止", "资源组替换", "配置漂移", "请求取消"} {
		t.Run(mode, func(t *testing.T) {
			fixture, probes, _, resource, current := newProbeFixture(t)
			expected, err := probes.parent(t.Context(), resource, current.ID)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			original := time.Now().Add(time.Second)
			converted := agentchannel.Deadline{ExpiresNS: 123456789, TimeNamespace: 5678}
			calls := 0
			locate := probes.locate
			fixture.node.stampDeadline = func(deadline time.Time, pid int) (agentchannel.Deadline, error) {
				calls++
				if deadline != original || pid != current.State.Pid {
					t.Fatal("未传递原期限和已核验的容器主进程", deadline, pid)
				}
				changed := current
				switch mode {
				case "时钟失败":
					return agentchannel.Deadline{}, errors.New("测试时钟不可读取")
				case "进程替换":
					changed.State.Pid++
				case "容器停止":
					changed.State.Running = false
				case "资源组替换":
					locate = func(context.Context, string, int) (processgroup.ContainerReference, error) {
						changed := expected
						changed.Inode++
						return changed, nil
					}
				case "配置漂移":
					changed.HostConfig.Privileged = true
				case "请求取消":
					cancel()
				}
				fixture.engine.containers[resource.Name] = changed
				return converted, nil
			}
			stamp, err := fixture.node.serviceDeadline(ctx, resource, expected, func(ctx context.Context, id string, pid int) (processgroup.ContainerReference, error) {
				return locate(ctx, id, pid)
			}, original)
			if calls != 1 {
				t.Fatal("时钟转换重复执行", calls)
			}
			if mode == "正常换算" {
				if err != nil || stamp != converted {
					t.Fatal("未保留目标进程的换算期限", stamp, err)
				}
			} else if err == nil || stamp != (agentchannel.Deadline{}) {
				t.Fatal("漂移或失败后仍签发期限", stamp, err)
			}
		})
	}
}
