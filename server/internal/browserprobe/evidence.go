// Package browserprobe 定义节点与受信浏览器代理之间的能力回执，不对业务窗口开放诊断入口。
package browserprobe

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/oklog/ulid/v2"
)

// Origin 使用保留域名；只有已通过节点对端身份校验的出网代理提供诊断页，不访问外部网站。
const Origin = "http://tombot-probe.invalid"

type Evidence struct {
	ChallengeID   string `json:"challenge_id"`
	Sandbox       bool   `json:"sandbox"`
	Windows       bool   `json:"windows"`
	SharedStorage bool   `json:"shared_storage"`
	Rendering     bool   `json:"rendering"`
	Input         bool   `json:"input"`
	Proxy         bool   `json:"proxy"`
	Cleanup       bool   `json:"cleanup"`
}

// Failure 只跨进程传递固定分类，不包含页面正文、网址或底层协议错误。
type Failure struct{ Code string }

func (failure *Failure) Error() string { return "浏览器能力检查失败：" + failure.Code }
func ValidFailure(code string) bool {
	switch code {
	case "browser_probe_windows_failed", "browser_probe_proxy_failed", "browser_probe_rendering_failed", "browser_probe_input_failed", "browser_probe_storage_failed", "browser_probe_sandbox_failed", "browser_probe_cleanup_failed":
		return true
	}
	return false
}

// Validate 拒绝旧挑战、部分检查和未确认清理的结果。
func (e Evidence) Validate(challenge string) error {
	if _, err := ulid.ParseStrict(challenge); err != nil || e.ChallengeID != challenge || !e.Sandbox || !e.Windows || !e.SharedStorage || !e.Rendering || !e.Input || !e.Proxy || !e.Cleanup {
		return errors.New("浏览器能力证据不完整或挑战不符")
	}
	return nil
}

// Serve 只应在出网代理完成连接身份、协议和目标一致性检查后调用；返回是否为诊断域名。
func Serve(w http.ResponseWriter, r *http.Request) (bool, error) {
	if r.URL.Hostname() != "tombot-probe.invalid" {
		return false, nil
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "close")
	challenge := strings.TrimPrefix(r.URL.Path, "/")
	_, err := ulid.ParseStrict(challenge)
	if err != nil || r.Method != http.MethodGet || r.URL.Host != "tombot-probe.invalid" || r.URL.Scheme != "http" || r.URL.RawQuery != "" || r.URL.RawPath != "" || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		w.WriteHeader(http.StatusBadRequest)
		return true, nil
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// 固定布局供真实截图、鼠标和键盘探针核验；挑战经过严格 ULID 校验，不包含用户内容。
	_, err = io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><title>浏览器能力检查</title><style>html,body{margin:0;background:rgb(23,137,211)}input{position:absolute;left:20px;top:20px;width:220px;height:40px}</style></head><body data-challenge="`+challenge+`"><input aria-label="能力检查输入框" oninput="document.body.dataset.input=this.value"></body></html>`)
	return true, err
}
