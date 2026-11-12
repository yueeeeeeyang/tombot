package admin

import (
	"bytes"
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func buildFixture() fstest.MapFS {
	return fstest.MapFS{
		"index.html":             {Data: []byte(`<html lang="zh-CN"><script type="module" src="/admin/assets/index-build.js"></script></html>`)},
		"assets/index-build.js":  {Data: []byte(`document.title='TomBot 管理平台'`)},
		"assets/index-build.css": {Data: []byte(`:root { color-scheme:light; }`)},
	}
}

func TestAdminAssets(t *testing.T) {
	fixture := buildFixture()
	fixture["internal.txt"] = &fstest.MapFile{Data: []byte("不应通过静态入口公开")}
	handler, err := New(fixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/admin/", 200}, {"HEAD", "/admin/", 200}, {"GET", "/admin/assets/index-build.js", 200}, {"GET", "/admin/assets/index-build.css", 200},
		{"GET", "/admin", 308}, {"GET", "/admin/assets/", 404}, {"GET", "/admin/internal.txt", 404}, {"GET", "/admin/../internal.txt", 404},
		{"GET", "/admin/assets/%69ndex-build.js", 404}, {"GET", "/admin/missing", 404}, {"POST", "/admin/", 405},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != test.code {
			t.Fatal(test, response.Code)
		}
		if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(response.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatal("静态响应缺少边界头", test)
		}
		if test.method == "HEAD" && response.Body.Len() != 0 {
			t.Fatal("HEAD 返回正文")
		}
		if strings.Contains(response.Body.String(), "不应通过静态入口公开") {
			t.Fatal("读取了非构建资源")
		}
	}
	fixture["assets/index-build.js"].Data = []byte("构造完成后的文件变化")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/admin/assets/index-build.js", nil))
	if strings.Contains(response.Body.String(), "构造完成后的文件变化") {
		t.Fatal("请求期重新读取了构建文件")
	}
}

func TestAdminBuildValidation(t *testing.T) {
	for name, change := range map[string]func(fstest.MapFS){
		"缺失入口":   func(root fstest.MapFS) { delete(root, "index.html") },
		"入口路径不符": func(root fstest.MapFS) { root["index.html"].Data = []byte("没有管理资源路径") },
		"超大入口":   func(root fstest.MapFS) { root["index.html"].Data = bytes.Repeat([]byte("x"), 65<<10) },
		"空资源":    func(root fstest.MapFS) { root["assets/index-build.js"].Data = nil },
		"超大资源":   func(root fstest.MapFS) { root["assets/index-build.js"].Data = bytes.Repeat([]byte("x"), (8<<20)+1) },
		"源文件":    func(root fstest.MapFS) { root["assets/source.ts"] = &fstest.MapFile{Data: []byte("源文件")} },
		"符号链接":   func(root fstest.MapFS) { root["assets/index-build.js"].Mode = fs.ModeSymlink },
	} {
		t.Run(name, func(t *testing.T) {
			root := buildFixture()
			change(root)
			if _, err := New(root); err == nil {
				t.Fatal("不完整或不安全构建被接受")
			}
		})
	}
	if _, err := New(nil); err == nil {
		t.Fatal("缺少构建仍启动")
	}
}
