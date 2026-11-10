package httpapi

import (
	"html"
	"net/http"
	"strings"
)

func (s *Server) openComputer(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDesktop(); err != nil {
		s.fail(w, r, err)
		return
	}
	var request struct {
		AgentID string `json:"agentId"`
	}
	if err := decode(w, r, &request); err != nil {
		s.fail(w, r, err)
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	url, err := s.Workspace.OpenComputer(r.Context(), request.AgentID, scheme+"://"+r.Host)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, http.StatusOK, map[string]string{"url": url})
}

func (s *Server) computerView(w http.ResponseWriter, r *http.Request) {
	if s.Workspace == nil {
		http.Error(w, "电脑服务还没有启动", http.StatusServiceUnavailable)
		return
	}
	agentID := r.URL.Query().Get("agent")
	botID, err := s.Workspace.ComputerAccess(agentID, r.URL.Query().Get("ticket"))
	if err != nil {
		http.Error(w, "电脑连接已失效，请重新连接。", http.StatusUnauthorized)
		return
	}
	phase := "尚未开始"
	if s.Provision != nil {
		phase = provisionPhase(s.Provision.Snapshot().Phase)
	}
	files := "工作区还没有文件。"
	if s.Files != nil {
		listing, listErr := s.Files.Tree(r.Context(), botID, "bot", ".", 1)
		if listErr != nil {
			files = "暂时无法读取工作区。"
		} else if len(listing.Entries) > 0 {
			names := make([]string, 0, len(listing.Entries))
			for _, entry := range listing.Entries {
				names = append(names, html.EscapeString(entry.Path))
			}
			files = strings.Join(names, "、")
		}
	}
	browser := "持久浏览器还没就绪。引导完成后，agent 会在这里使用它。"
	if s.Browser != nil {
		if status, statusErr := s.Browser.Status(r.Context()); statusErr == nil && status.Available {
			browser = "持久浏览器可用，agent 正在使用这台电脑。"
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>电脑</title>
<style>body{font-family:-apple-system,sans-serif;margin:16px;color:#252525;background:#fdfdfd}h1{font-size:16px}section{border:1px solid #e8e8e8;border-radius:12px;padding:12px;margin:0 0 10px;background:#fff}p{margin:6px 0;font-size:13px;line-height:1.6}</style>
</head><body><h1>这只猫咪的电脑</h1>
<section><h1>引导</h1><p>` + html.EscapeString(phase) + `</p></section>
<section><h1>浏览器</h1><p>` + html.EscapeString(browser) + `</p></section>
<section><h1>文件</h1><p>` + files + `</p></section>
<section><h1>终端</h1><p>终端由服务端 agent 使用，离开这个页面不会结束会话。</p></section>
</body></html>`
	if _, err = w.Write([]byte(page)); err != nil && s.Logger != nil {
		s.Logger.Warn("电脑页写出失败", "agent_id", agentID, "bot_id", botID, "error", err)
	}
}

func provisionPhase(phase string) string {
	switch phase {
	case "", "pending":
		return "尚未开始"
	case "detecting":
		return "正在检查这台电脑"
	case "preparing", "selfcheck":
		return "正在准备"
	case "ready":
		return "已就绪"
	case "failed":
		return "准备失败"
	default:
		return "进行中"
	}
}
