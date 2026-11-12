package computer

import (
	"encoding/json"
	"strings"
	"testing"

	"io.yueyang/tom/internal/infra/ids"
)

func TestEgressEnvironmentUsesOneLoopbackEndpoint(t *testing.T) {
	address, err := EgressAddress()
	if err != nil || address != "127.0.0.1:3128" {
		t.Fatal("环回代理端点错误", err)
	}
	var environment map[string]string
	if err := json.Unmarshal([]byte(desktopEnvironmentTemplate), &environment); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"http_proxy", "https_proxy", "HTTP_PROXY", "HTTPS_PROXY"} {
		if environment[name] != "http://"+address {
			t.Fatal("代理地址与桌面模板不一致", name)
		}
	}
	if environment["no_proxy"] != "127.0.0.1,localhost,::1" || environment["NO_PROXY"] != environment["no_proxy"] {
		t.Fatal("本机服务排除范围错误")
	}
}

func TestDesktopEnvironmentSharesToolsAndSeparatesControl(t *testing.T) {
	t.Setenv("PARENT_PRIVATE", "parent-only-example")
	first := ExecutionSession{DesktopIdentity: DesktopIdentity{UserID: ids.New(), ComputerID: ids.New(), BotID: ids.New(), DesktopID: ids.New(), UID: DesktopUIDBase + 1, GID: DesktopSharedGID, DisplayNumber: 1}, NodeID: ids.New(), ContainerID: strings.Repeat("a", 64), Generation: 1, SessionID: ids.New(), RunID: ids.New(), FencingToken: 1}
	firstEnvironment, err := first.DesktopEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if firstEnvironment["PARENT_PRIVATE"] != "" || firstEnvironment["HOME"] != "/workspace" || firstEnvironment["TMPDIR"] != "/workspace/.tmp" || firstEnvironment["DISPLAY"] != ":1" || firstEnvironment["USER"] != "bot100001" {
		t.Fatal("桌面环境没有正确绑定稳定身份和共享路径")
	}
	second := first
	second.BotID, second.DesktopID, second.SessionID, second.RunID = ids.New(), ids.New(), ids.New(), ids.New()
	second.UID, second.DisplayNumber = DesktopUIDBase+2, 2
	secondEnvironment, err := second.DesktopEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"HOME", "TMPDIR", "PATH", "GOPATH", "CARGO_HOME", "UV_TOOL_BIN_DIR", "UV_TOOL_DIR", "NPM_CONFIG_PREFIX", "PNPM_HOME", "BUN_INSTALL"} {
		if firstEnvironment[name] == "" || firstEnvironment[name] != secondEnvironment[name] {
			t.Fatal("同用户的业务或工具环境不共享", name)
		}
	}
	for _, directory := range []string{"/workspace/tools/bin", "/workspace/tools/node/bin", "/workspace/tools/pnpm", "/workspace/tools/bun/bin", "/workspace/tools/go/bin", "/workspace/tools/cargo/bin"} {
		if !strings.Contains(":"+firstEnvironment["PATH"]+":", ":"+directory+":") {
			t.Fatal("共享工具缺少命令搜索路径", directory)
		}
	}
	for _, name := range []string{"XAUTHORITY", "ICEAUTHORITY", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "NSS_WRAPPER_PASSWD", "NSS_WRAPPER_GROUP", "DBUS_SESSION_BUS_ADDRESS"} {
		if !strings.Contains(firstEnvironment[name], first.RuntimeDirectory()) || !strings.Contains(secondEnvironment[name], second.RuntimeDirectory()) || firstEnvironment[name] == secondEnvironment[name] {
			t.Fatal("图形控制环境没有独立绑定执行会话", name)
		}
	}
	if secondEnvironment["DISPLAY"] != ":2" || secondEnvironment["USER"] != "bot100002" {
		t.Fatal("第二桌面的身份和显示编号错误")
	}
	// 同一 Bot 的下一次授权保持文件身份，但不继承旧会话控制路径。
	next := first
	next.SessionID, next.RunID = ids.New(), ids.New()
	next.FencingToken++
	nextEnvironment, err := next.DesktopEnvironment()
	if err != nil || nextEnvironment["USER"] != firstEnvironment["USER"] || nextEnvironment["DISPLAY"] != firstEnvironment["DISPLAY"] || nextEnvironment["XDG_RUNTIME_DIR"] == firstEnvironment["XDG_RUNTIME_DIR"] {
		t.Fatal("下一次授权没有保留稳定身份或更换运行目录", err)
	}
	firstEnvironment["HOME"] = "/changed"
	actual, err := first.DesktopEnvironment()
	if err != nil || actual["HOME"] != "/workspace" {
		t.Fatal("调用方修改污染了后续环境", err)
	}
	first.SessionID = "../foreign"
	if _, err := first.DesktopEnvironment(); err == nil {
		t.Fatal("未经验证的会话路径被插入环境")
	}
}
