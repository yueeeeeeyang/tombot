package processgroup

import (
	"strings"
	"testing"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

func TestGroupReference(t *testing.T) {
	containerID := strings.Repeat("a", 64)
	reference := Reference{Session: computer.ExecutionSession{DesktopIdentity: computer.DesktopIdentity{UserID: ids.New(), ComputerID: ids.New(), BotID: ids.New(), DesktopID: ids.New(), UID: computer.DesktopUIDBase + 1, GID: computer.DesktopSharedGID, DisplayNumber: 1}, NodeID: ids.New(), ContainerID: containerID, Generation: 1, SessionID: ids.New(), RunID: ids.New(), FencingToken: 1}, ParentPath: "system.slice/docker-" + containerID + ".scope", ParentDevice: 1, ParentInode: 2}
	if reference.Validate() != nil || strings.Contains(reference.Name(), "/") {
		t.Fatal("合法资源组被拒绝")
	}
	for _, path := range []string{"", "/system.slice/docker-" + containerID + ".scope", "../docker/" + containerID, "docker/../" + containerID, "docker/" + strings.Repeat("b", 64), "docker/" + containerID + "/child", "docker\\" + containerID} {
		changed := reference
		changed.ParentPath = path
		if changed.Validate() == nil {
			t.Fatal("接受错误父资源组", path)
		}
	}
	changed := reference
	changed.ParentInode = 0
	if changed.Validate() == nil {
		t.Fatal("接受未绑定的父资源组")
	}
}

func TestContainerReferenceRequiresExactParent(t *testing.T) {
	id := strings.Repeat("a", 64)
	valid := ContainerReference{ContainerID: id, Path: "system.slice/docker-" + id + ".scope", Device: 1, Inode: 2}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ContainerReference){
		func(r *ContainerReference) { r.ContainerID = strings.Repeat("z", 64) },
		func(r *ContainerReference) { r.ContainerID = strings.Repeat("b", 64) },
		func(r *ContainerReference) { r.Path += "/child" },
		func(r *ContainerReference) { r.Path = "/" + r.Path },
		func(r *ContainerReference) { r.Device = 0 },
		func(r *ContainerReference) { r.Inode = 0 },
	} {
		value := valid
		change(&value)
		if value.Validate() == nil {
			t.Fatal("错误容器父组被接受", value)
		}
	}
}
func TestKernelGroupParsers(t *testing.T) {
	for _, input := range []string{"", "0::/", "0::/a/../b", "0:cpu:/docker/id", "0::/docker/a\n0::/docker/b", "invalid"} {
		if _, err := unifiedPath([]byte(input)); err == nil {
			t.Fatal("接受无效进程归属", input)
		}
	}
	for input, want := range map[string]string{"0::/system.slice/docker-id.scope\n": "system.slice/docker-id.scope", "3:cpu:/legacy\n0::/docker/id\n": "docker/id"} {
		if actual, err := unifiedPath([]byte(input)); err != nil || actual != want {
			t.Fatal(actual, err)
		}
	}
	for _, input := range []string{"", "frozen 0", "populated 2", "populated 0\npopulated 1", "populated"} {
		if _, err := populated([]byte(input)); err == nil {
			t.Fatal("接受无效存活状态", input)
		}
	}
	for input, want := range map[string]bool{"populated 0\nfrozen 0\n": false, "populated 1\nfrozen 1\n": true} {
		if actual, err := populated([]byte(input)); err != nil || actual != want {
			t.Fatal(actual, err)
		}
	}
	for _, input := range []string{"1", "0", "-2", "bad", "2\n3\n4"} {
		if _, err := processIDs([]byte(input), 2); err == nil {
			t.Fatal("接受无效进程列表", input)
		}
	}
	if pids, err := processIDs([]byte("2\n1042\n"), 2); err != nil || len(pids) != 2 || pids[1] != 1042 {
		t.Fatal(pids, err)
	}
}
