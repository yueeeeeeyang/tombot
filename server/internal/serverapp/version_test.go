package serverapp

import (
	"encoding/json"
	"net/http/httptest"
	"runtime/debug"
	"testing"
)

func TestVersionFromBuild(t *testing.T) {
	for _, fixture := range []struct {
		name, release, expected string
		build                   *debug.BuildInfo
	}{
		{name: "显式发布版本", release: "2.3.4", expected: "2.3.4"},
		{name: "模块版本", build: &debug.BuildInfo{Main: debug.Module{Version: "v2.3.4"}}, expected: "v2.3.4"},
		{name: "干净提交", build: &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abcdef0123456789"}}}, expected: "abcdef012345"},
		{name: "未提交修改", build: &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.modified", Value: "true"}, {Key: "vcs.revision", Value: "abcdef0"}}}, expected: "abcdef0-dirty"},
		{name: "缺少构建信息"},
		{name: "缺少提交信息", build: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{{Key: "vcs.modified", Value: "true"}}}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if actual := versionFromBuild(fixture.release, fixture.build); actual != fixture.expected {
				t.Fatalf("构建版本 %q，预期 %q", actual, fixture.expected)
			}
		})
	}
}

func TestVersionHandler(t *testing.T) {
	for _, fixture := range []struct {
		method, version string
		status          int
	}{
		{"GET", "2.3.4", 200},
		{"HEAD", "2.3.4", 200},
		{"GET", "", 503},
		{"POST", "2.3.4", 405},
	} {
		t.Run(fixture.method+fixture.version, func(t *testing.T) {
			response := httptest.NewRecorder()
			versionHandler(fixture.version, testLogger())(response, httptest.NewRequest(fixture.method, "/api/v2/version", nil))
			if response.Code != fixture.status || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("版本响应错误：%d %v", response.Code, response.Header())
			}
			if fixture.method == "GET" && fixture.status == 200 {
				var result map[string]string
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result) != 1 || result["server_version"] != fixture.version {
					t.Fatal("版本响应不符或包含无关构建信息")
				}
			}
			if fixture.method == "HEAD" && response.Body.Len() != 0 {
				t.Fatal("HEAD 响应包含正文")
			}
			if fixture.status == 405 && response.Header().Get("Allow") != "GET, HEAD" {
				t.Fatal("方法拒绝未说明允许的方法")
			}
		})
	}
}
