package taskprocess

import (
	"slices"
	"strings"
	"testing"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

func browserRequest() BrowserRequest {
	probe := probeRequest()
	probe.Group.Kind = processgroup.BrowserService
	return BrowserRequest{Version: 1, Group: probe.Group, Deadline: probe.Deadline}
}

func TestBrowserAuthorizationAndFixedInvocation(t *testing.T) {
	request := browserRequest()
	encoded, err := encodeBrowser(request)
	if err != nil {
		t.Fatal(err)
	}
	if actual, err := DecodeBrowser(encoded); err != nil || actual != request {
		t.Fatal("浏览器授权未完整往返", err)
	}
	for _, mutate := range []func(*BrowserRequest){
		func(r *BrowserRequest) { r.Version++ },
		func(r *BrowserRequest) { r.Group.Kind = processgroup.ProbeService },
		func(r *BrowserRequest) { r.Group.Container.Inode = 0 },
		func(r *BrowserRequest) { r.Group.ID = "../foreign" },
		func(r *BrowserRequest) { r.Deadline.ExpiresNS = 0 },
		func(r *BrowserRequest) { r.Deadline.TimeNamespace = 0 },
	} {
		changed := request
		mutate(&changed)
		if _, err := encodeBrowser(changed); err == nil {
			t.Fatal("错误浏览器身份被封存")
		}
	}
	for _, value := range []string{
		"{}", "null", string(encoded) + "{}", strings.Repeat(" ", 16<<10) + string(encoded),
		strings.Replace(string(encoded), `"version":1`, `"version":2,"version":1`, 1),
		strings.Replace(string(encoded), `"version":1`, `"Version":1`, 1),
		strings.Replace(string(encoded), `"version":1`, `"version":1,"uid":0`, 1),
		strings.Replace(string(encoded), `"version":1`, `"version":1,"program":"/bin/sh"`, 1),
		strings.Replace(string(encoded), `"version":1`, `"version":1,"profile":"/workspace"`, 1),
	} {
		if _, err := DecodeBrowser([]byte(value)); err == nil {
			t.Fatal("接受歧义或覆盖浏览器配置的授权")
		}
	}
	if _, err := DecodeProbe(encoded); err == nil {
		t.Fatal("浏览器授权被用于能力探针")
	}
	arguments, environment, err := browserCommand(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"--remote-debugging-pipe", "--proxy-server=http://127.0.0.1:3128", "--proxy-bypass-list=<-loopback>", "--user-data-dir=" + computer.BrowserDirectory} {
		if !slices.Contains(arguments, required) {
			t.Fatal("浏览器缺少固定启动边界", required)
		}
	}
	for _, value := range arguments {
		if strings.HasPrefix(value, "--no-sandbox") || strings.HasPrefix(value, "--remote-debugging-port") || strings.HasPrefix(value, "--proxy-pac-url") {
			t.Fatal("浏览器绕过隔离或开放网络调试")
		}
	}
	if arguments[0] != BrowserProgram || arguments[len(arguments)-1] != "about:blank" || !slices.Contains(environment, "HOME="+computer.BrowserDirectory) || !slices.Contains(environment, "TMPDIR="+request.RuntimeDirectory()) {
		t.Fatal("浏览器程序、初始页面或运行目录可被覆盖")
	}
}
