package serverapp

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// 发布脚本通过链接参数写入版本；直接构建时读取 Go 内置提交信息，不依赖运行目录。
var releaseVersion string

func versionFromBuild(release string, build *debug.BuildInfo) string {
	if release != "" {
		return release
	}
	if build == nil {
		return ""
	}
	if build.Main.Version != "" && build.Main.Version != "(devel)" {
		return build.Main.Version
	}
	var revision string
	var modified bool
	for _, setting := range build.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return ""
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified {
		revision += "-dirty"
	}
	return revision
}

// versionHandler 只公开构建版本；无元数据时明确报错，不把未知版本伪装成发布版本。
func versionHandler(version string, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "仅允许读取服务版本", http.StatusMethodNotAllowed)
			return
		}
		if version == "" {
			http.Error(w, "服务端构建未提供版本信息", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method == http.MethodHead {
			return
		}
		if err := json.NewEncoder(w).Encode(struct {
			Version string `json:"server_version"`
		}{Version: version}); err != nil {
			logger.WarnContext(r.Context(), "服务版本响应未发送完成", "path", "/api/v2/version")
		}
	}
}
