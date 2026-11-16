package bundle

import (
	"bytes"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedAdminWithoutExternalDirectory(t *testing.T) {
	// 在全新工作目录放置同名伪页面，确认正式资源只来自编译产物。
	directory := t.TempDir()
	t.Chdir(directory)
	if err := os.Mkdir("admin", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("admin", "index.html"), []byte("外部页面不得被加载"), 0600); err != nil {
		t.Fatal(err)
	}
	handler, err := New()
	if err != nil {
		t.Fatal(err)
	}
	root, err := fs.Sub(distribution, "dist")
	if err != nil {
		t.Fatal(err)
	}
	index, err := fs.ReadFile(root, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/admin/", "/admin/users", "/admin/model-policies/01ARZ3NDEKTSV4RRFFQ69G5FAV/versions"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", route, nil))
		if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), index) || bytes.Contains(response.Body.Bytes(), []byte("外部页面")) {
			t.Fatal("内置页面依赖工作目录或外部产物", route)
		}
		if !strings.Contains(response.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatal("页面缺少 CSP")
		}
	}
	entries, err := fs.ReadDir(root, "assets")
	if err != nil || len(entries) == 0 {
		t.Fatal("内置资源为空", err)
	}
	for _, entry := range entries {
		content, err := fs.ReadFile(root, "assets/"+entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/admin/assets/"+entry.Name(), nil))
		if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), content) {
			t.Fatal("内置资源缺失或被替换", entry.Name())
		}
	}
	for _, route := range []string{"/admin/assets/missing.js", "/admin/dist/index.html", "/admin/missing"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", route, nil))
		if response.Code != 404 {
			t.Fatal("未知路径错误回退页面", route)
		}
	}
}

func TestEmbeddedLoginAssets(t *testing.T) {
	handler, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ path, contentType, marker string }{
		{"/admin/assets/login.css", "text/css", "prefers-color-scheme: dark"},
		{"/admin/assets/user-platform.css", "text/css", ".user-shell"},
		{"/admin/assets/user-platform.js", "text/javascript", "tombot-user-theme"},
		{"/admin/assets/user-profile.js", "text/javascript", "cropRectangle"},
		{"/admin/assets/avatar-crop.js", "text/javascript", "readAvatarDimensions"},
		{"/admin/assets/profile-save.js", "text/javascript", "saveProfile"},
		{"/admin/assets/login-logo.png", "image/png", "PNG"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", test.path, nil))
		if response.Code != 200 || !strings.HasPrefix(response.Header().Get("Content-Type"), test.contentType) || !strings.Contains(response.Body.String(), test.marker) {
			t.Fatal("内置登录资源缺失或类型无效", test.path)
		}
	}
}
