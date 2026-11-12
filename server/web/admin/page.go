// Package admin 提供与管理 API 同源部署的管理前端静态资源，不承载业务或身份逻辑。
package admin

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"time"
)

type asset struct {
	content  []byte
	typeName string
}

// Handler 在启动时读取限定构建产物，运行期不会根据请求路径访问宿主文件系统。
type Handler struct{ assets map[string]asset }

// New 要求已完成 admin 构建。缺少入口、非普通文件或超出构建大小限制时拒绝启动。
func New(root fs.FS) (*Handler, error) {
	if root == nil {
		return nil, errors.New("管理前端构建目录未配置")
	}
	rootEntries, err := fs.ReadDir(root, ".")
	if err != nil {
		return nil, errors.New("管理前端构建目录不可读取")
	}
	for _, entry := range rootEntries {
		if entry.Name() == "index.html" && !entry.Type().IsRegular() || entry.Name() == "assets" && !entry.IsDir() {
			return nil, errors.New("管理前端构建目录类型无效")
		}
	}
	index, err := readAsset(root, "index.html", 64<<10)
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(index, []byte("/admin/assets/")) {
		return nil, errors.New("管理前端构建入口路径无效")
	}
	entries, err := fs.ReadDir(root, "assets")
	if err != nil || len(entries) == 0 || len(entries) > 256 {
		return nil, errors.New("管理前端静态资源目录无效")
	}
	assets := map[string]asset{"/admin/": {index, "text/html; charset=utf-8"}}
	validName := regexp.MustCompile(`^[A-Za-z0-9_-]+\.(js|css|woff2|png|svg|ico)$`)
	total := len(index)
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !validName.MatchString(entry.Name()) {
			return nil, errors.New("管理前端包含未允许的构建文件")
		}
		name := "assets/" + entry.Name()
		content, err := readAsset(root, name, 8<<20)
		if err != nil {
			return nil, err
		}
		total += len(content)
		if total > 16<<20 {
			return nil, errors.New("管理前端构建资源过大")
		}
		contentType := mime.TypeByExtension(path.Ext(name))
		if contentType == "" {
			return nil, errors.New("管理前端资源类型无法识别")
		}
		assets["/admin/"+name] = asset{content, contentType}
	}
	return &Handler{assets}, nil
}

func readAsset(root fs.FS, name string, maximum int) (content []byte, result error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, errors.New("管理前端构建文件不可读取")
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	stat, statErr := file.Stat()
	if statErr != nil || !stat.Mode().IsRegular() || stat.Size() < 1 || stat.Size() > int64(maximum) {
		return nil, errors.New("管理前端构建文件类型或大小无效")
	}
	content, err = io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil || len(content) < 1 || len(content) > maximum {
		return nil, errors.New("读取管理前端构建文件失败")
	}
	return content, nil
}

// ServeHTTP 只服务明确的入口与构建资源；未知路径不回退 HTML，不提供目录列表或任意文件读取。
func (handler *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "请求方式不支持", 405)
		return
	}
	if r.URL.Path == "/admin" {
		http.Redirect(w, r, "/admin/", http.StatusPermanentRedirect)
		return
	}
	value, ok := handler.assets[r.URL.Path]
	if !ok || r.URL.RawPath != "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", value.typeName)
	http.ServeContent(w, r, path.Base(r.URL.Path), time.Time{}, bytes.NewReader(value.content))
}
