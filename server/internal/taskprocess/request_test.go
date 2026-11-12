package taskprocess

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
)

func validRequest() Request {
	containerID := strings.Repeat("a", 64)
	return Request{Version: 2, OperationID: ids.New(), Group: processgroup.Reference{Session: computer.ExecutionSession{DesktopIdentity: computer.DesktopIdentity{UserID: ids.New(), ComputerID: ids.New(), BotID: ids.New(), DesktopID: ids.New(), UID: computer.DesktopUIDBase + 1, GID: computer.DesktopSharedGID, DisplayNumber: 1}, NodeID: ids.New(), ContainerID: containerID, Generation: 1, SessionID: ids.New(), RunID: ids.New(), FencingToken: 1}, ParentPath: "system.slice/docker-" + containerID + ".scope", ParentDevice: 1, ParentInode: 2}, Role: "shell", ProcessCommand: computer.ProcessCommand{WorkingDirectory: "/workspace", Program: "/bin/sh", Arguments: []string{"-c", "printf example"}, Environment: map[string]string{"EXAMPLE": "task value"}}}
}

func TestRequestProtocolAndBoundaries(t *testing.T) {
	request := validRequest()
	encoded, err := encode(request)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := decode(encoded)
	if err != nil || !reflect.DeepEqual(actual, request) {
		t.Fatalf("规范内容未往返：%v", err)
	}
	cases := []struct {
		name   string
		change func(*Request)
	}{
		{"旧版本", func(r *Request) { r.Version = 1 }},
		{"未绑定会话", func(r *Request) { r.Group.Session.SessionID = "" }},
		{"无效父组", func(r *Request) { r.Group.ParentPath = "../" + r.Group.ParentPath }},
		{"无操作", func(r *Request) { r.OperationID = "" }},
		{"任意组", func(r *Request) { r.Role = "other" }},
		{"特权身份", func(r *Request) { r.Group.Session.UID, r.Group.Session.GID = 0, 0 }},
		{"保留身份", func(r *Request) { r.Group.Session.UID, r.Group.Session.GID = 9999, 9999 }},
		{"身份溢出", func(r *Request) { r.Group.Session.UID, r.Group.Session.GID = 1<<31, 1<<31 }},
		{"跨任务组", func(r *Request) { r.Group.Session.GID++ }},
		{"相对目录", func(r *Request) { r.WorkingDirectory = "relative" }},
		{"目录逃逸", func(r *Request) { r.WorkingDirectory = "../other" }},
		{"非规范目录", func(r *Request) { r.WorkingDirectory = "/one/../two" }},
		{"路径空字节", func(r *Request) { r.WorkingDirectory = "a\x00b" }},
		{"隐式程序查找", func(r *Request) { r.Program = "sh" }},
		{"程序空字节", func(r *Request) { r.Program = "/bin/sh\x00" }},
		{"参数空字节", func(r *Request) { r.Arguments = []string{"a\x00b"} }},
		{"参数编码", func(r *Request) { r.Arguments = []string{string([]byte{0xff})} }},
		{"参数数量", func(r *Request) { r.Arguments = make([]string, 257) }},
		{"环境名称", func(r *Request) { r.Environment = map[string]string{"A=B": "x"} }},
		{"覆盖目录", func(r *Request) { r.Environment = map[string]string{"HOME": "/"} }},
		{"覆盖身份", func(r *Request) { r.Environment = map[string]string{"USER": "root"} }},
		{"内容上限", func(r *Request) { r.Arguments = []string{strings.Repeat("x", computer.MaximumProcessBytes)} }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			changed := validRequest()
			test.change(&changed)
			if _, err := encode(changed); err == nil {
				t.Fatal("非法启动内容被接受")
			}
		})
	}
	for _, name := range []string{"HOME", "TMPDIR", "TMP", "TEMP", "USER", "LOGNAME", "DISPLAY", "XAUTHORITY", "ICEAUTHORITY", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "NSS_WRAPPER_PASSWD", "NSS_WRAPPER_GROUP"} {
		t.Run("受管环境_"+name, func(t *testing.T) {
			changed := validRequest()
			changed.Environment[name] = "foreign-desktop"
			if _, err := encode(changed); err == nil {
				t.Fatal("接受了受管身份或桌面环境覆盖", name)
			}
		})
	}
	for _, name := range []string{"uid", "gid"} {
		legacy := bytes.Replace(encoded, []byte(`"version":2`), []byte(`"version":2,"`+name+`":100001`), 1)
		if _, err := decode(legacy); err == nil {
			t.Fatal("接受了重复声明的任务身份", name)
		}
	}
	for _, directory := range []string{"/", "/workspace", "/workspace/另一机器人产出", "/usr/share"} {
		changed := validRequest()
		changed.WorkingDirectory = directory
		if _, err := encode(changed); err != nil {
			t.Fatal("规范绝对目录被错误限制到单任务目录", err)
		}
	}
	for _, encoded := range [][]byte{nil, []byte("null"), []byte("[]"), append(append([]byte{}, encoded...), []byte(" {}")...), append([]byte{' '}, encoded...), bytes.Replace(encoded, []byte(`"version":2`), []byte(`"version":2,"version":2`), 1), bytes.Replace(encoded, []byte(`"version":2`), []byte(`"version":2,"unexpected":true`), 1), bytes.Replace(encoded, []byte(`"version":2`), []byte(`"Version":2`), 1), bytes.Repeat([]byte("x"), maximumRequestBytes+1)} {
		if _, err := decode(encoded); err == nil {
			t.Fatal("非规范协议被接受")
		}
	}
	request = validRequest()
	request.Arguments = []string{strings.Repeat("x", computer.MaximumProcessBytes-len(request.Program)-2)}
	request.Environment = nil
	if err := request.Validate(); err != nil {
		t.Fatal("合法边界被拒绝", err)
	}
	request.Arguments[0] += "x"
	if err := request.Validate(); err == nil {
		t.Fatal("未计算参数终止字节")
	}
	request = validRequest()
	request.Arguments = []string{strings.Repeat("\n", 50000)}
	if _, err := encode(request); err == nil {
		t.Fatal("转义后协议总量未限制")
	}
}

func TestTaskEnvironmentSharesFilesAndBindsDesktop(t *testing.T) {
	t.Setenv("PARENT_ONLY", "must remain in parent")
	request := validRequest()
	request.Environment["LANG"] = "C"
	values := make(map[string]string)
	environment, err := request.environment()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range environment {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	if _, ok := values["PARENT_ONLY"]; ok {
		t.Fatal("继承了父进程环境")
	}
	if values["HOME"] != "/workspace" || values["TMPDIR"] != "/workspace/.tmp" || values["DISPLAY"] != ":1" || values["XDG_RUNTIME_DIR"] != request.Group.Session.RuntimeDirectory() || values["EXAMPLE"] != "task value" || values["LANG"] != "C" {
		t.Fatal("任务环境或目录绑定错误")
	}
	request.Role = "browser"
	if err := request.Validate(); err != nil {
		t.Fatal("浏览器资源组无法使用已授权桌面身份", err)
	}
	other, err := request.environment()
	if err != nil || !reflect.DeepEqual(environment, other) {
		t.Fatal("不同资源组改变了共享业务路径或桌面路由", err)
	}
	// 编码内容可能包含用户命令，验证失败不能把它返回到控制日志。
	request.Program = "/invalid\x00private-example"
	_, err = encode(request)
	if err == nil || strings.Contains(err.Error(), "private-example") {
		t.Fatal("错误暴露了输入")
	}
}

func TestCanonicalEnvironmentOrder(t *testing.T) {
	request := validRequest()
	request.Environment = map[string]string{"Z": "last", "A": "first"}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decode(encoded); err != nil {
		t.Fatal(err)
	}
}
