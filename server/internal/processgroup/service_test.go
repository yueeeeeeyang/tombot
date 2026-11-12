package processgroup

import (
	"encoding/json"
	"strings"
	"testing"

	"io.yueyang/tom/internal/infra/ids"
)

func serviceFixture() ServiceReference {
	id := strings.Repeat("a", 64)
	return ServiceReference{Container: ContainerReference{ContainerID: id, Path: "system.slice/docker-" + id + ".scope", Device: 1, Inode: 2}, ID: ids.New(), Kind: BrowserService}
}

func TestServiceIdentity(t *testing.T) {
	reference := serviceFixture()
	for _, kind := range []ServiceKind{ProbeService, BrowserService} {
		reference.Kind = kind
		if err := reference.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for name, change := range map[string]func(*ServiceReference){
		"缺少实例":  func(r *ServiceReference) { r.ID = "" },
		"非规范实例": func(r *ServiceReference) { r.ID = "0000000000000000000000000a" },
		"实例溢出":  func(r *ServiceReference) { r.ID = "8" + strings.Repeat("0", 25) },
		"路径注入":  func(r *ServiceReference) { r.ID = "../" + r.ID },
		"缺少用途":  func(r *ServiceReference) { r.Kind = "" },
		"任务用途":  func(r *ServiceReference) { r.Kind = "shell" },
		"用途注入":  func(r *ServiceReference) { r.Kind = "browser/../probe" },
		"错误容器":  func(r *ServiceReference) { r.Container.ContainerID = strings.Repeat("b", 64) },
		"任务子组":  func(r *ServiceReference) { r.Container.Path += "/tombot-session-other" },
		"未固定设备": func(r *ServiceReference) { r.Container.Device = 0 },
		"未固定目录": func(r *ServiceReference) { r.Container.Inode = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			changed := reference
			change(&changed)
			if changed.Validate() == nil {
				t.Fatal("错误基础服务身份被接受")
			}
		})
	}
}

func TestServiceIdentityRoundTrip(t *testing.T) {
	reference := serviceFixture()
	encoded, err := json.Marshal(reference)
	if err != nil {
		t.Fatal(err)
	}
	var restored ServiceReference
	if err = json.Unmarshal(encoded, &restored); err != nil || restored != reference || restored.Validate() != nil {
		t.Fatal("基础服务持久身份丢失", err)
	}
	var record map[string]json.RawMessage
	if err = json.Unmarshal(encoded, &record); err != nil {
		t.Fatal(err)
	}
	var container map[string]json.RawMessage
	if err = json.Unmarshal(record["container"], &container); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"container_id", "path", "device", "inode"} {
		if _, exists := container[field]; !exists {
			t.Fatal("缺少固定容器身份字段", field)
		}
	}
}

func TestServiceGroupsHaveDisjointNames(t *testing.T) {
	first := serviceFixture()
	probe := first
	probe.Kind = ProbeService
	restarted := first
	restarted.ID = ids.New()
	names := make(map[string]bool)
	for _, reference := range []ServiceReference{first, probe, restarted} {
		name := reference.Name()
		if names[name] || strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, "tombot-session-") {
			t.Fatal("服务实例或任务目录发生重叠")
		}
		names[name] = true
	}
}
