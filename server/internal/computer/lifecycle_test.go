package computer

import (
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/infra/ids"
)

func validSpecification() Specification {
	userID := ids.New()
	return Specification{OperationID: ids.New(), UserID: userID, ComputerID: ids.New(), NodeID: ids.New(),
		ImageReference: "registry.example.test/computer@sha256:" + strings.Repeat("a", 64), VolumeReference: "tombot-user-" + userID,
		Generation: 1, FencingToken: 1, Attempt: 1, LeaseExpiresAt: time.Now().Add(time.Minute),
		Resources: Resources{CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 2 << 30, ProcessLimit: 128}}
}

func TestSpecificationBounds(t *testing.T) {
	if err := validSpecification().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Specification){
		"用户标识":     func(s *Specification) { s.UserID = "../other" },
		"浮动镜像":     func(s *Specification) { s.ImageReference = "registry.example.test/computer:latest" },
		"宿主路径":     func(s *Specification) { s.VolumeReference = "/var/lib/other" },
		"其他用户卷":    func(s *Specification) { s.VolumeReference = "tombot-user-" + ids.New() },
		"缺少租约":     func(s *Specification) { s.LeaseExpiresAt = time.Time{} },
		"无效代次":     func(s *Specification) { s.Generation = 0 },
		"无效控制者":    func(s *Specification) { s.FencingToken = 0 },
		"无效尝试号":    func(s *Specification) { s.Attempt = 0 },
		"无 CPU 限额": func(s *Specification) { s.Resources.CPUMillis = 0 },
		"超大内存":     func(s *Specification) { s.Resources.MemoryBytes = 1<<40 + 1 },
		"超小磁盘":     func(s *Specification) { s.Resources.DiskBytes = 512<<20 - 1 },
		"无进程限制":    func(s *Specification) { s.Resources.ProcessLimit = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			specification := validSpecification()
			change(&specification)
			if specification.Validate() == nil {
				t.Fatal("无效电脑参数被接受")
			}
		})
	}
}

func TestObservationIdentityAndState(t *testing.T) {
	specification := validSpecification()
	ready := Observation{OperationID: specification.OperationID, UserID: specification.UserID, ComputerID: specification.ComputerID,
		Generation: specification.Generation, FencingToken: specification.FencingToken, VolumeReference: specification.VolumeReference,
		State: "ready", ContainerID: strings.Repeat("a", 64), Capabilities: Capabilities{true, true, true, true, true, true}}
	if !ready.Matches(specification) || !ready.Capabilities.Ready() {
		t.Fatal("完整就绪观测未通过")
	}
	for name, change := range map[string]func(*Observation){
		"其他操作":      func(o *Observation) { o.OperationID = ids.New() },
		"其他用户":      func(o *Observation) { o.UserID = ids.New() },
		"其他电脑":      func(o *Observation) { o.ComputerID = ids.New() },
		"旧代次":       func(o *Observation) { o.Generation-- },
		"旧控制者":      func(o *Observation) { o.FencingToken-- },
		"其他卷":       func(o *Observation) { o.VolumeReference = "other" },
		"未知状态":      func(o *Observation) { o.State = "" },
		"就绪无容器":     func(o *Observation) { o.ContainerID = "" },
		"不存在却有容器":   func(o *Observation) { o.State = "absent" },
		"短容器标识":     func(o *Observation) { o.ContainerID = "abc" },
		"容器标识非十六进制": func(o *Observation) { o.ContainerID = strings.Repeat("g", 64) },
		"容器标识非规范形式": func(o *Observation) { o.ContainerID = strings.Repeat("A", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			observation := ready
			change(&observation)
			if observation.Matches(specification) {
				t.Fatal("错误观测被接受")
			}
		})
	}
	for _, state := range []string{"absent", "present", "quarantined"} {
		observation := ready
		observation.State, observation.ContainerID = state, ""
		if !observation.Matches(specification) {
			t.Fatal("部分资源状态未被识别", state)
		}
	}
	for index := range 6 {
		capabilities := ready.Capabilities
		fields := []*bool{&capabilities.Terminal, &capabilities.Browser, &capabilities.Files, &capabilities.ProcessReaping, &capabilities.DiskQuota, &capabilities.NetworkIsolation}
		*fields[index] = false
		if capabilities.Ready() {
			t.Fatal("缺少必要能力仍判定就绪", index)
		}
	}
}
