package taskprocess

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

const BrowserProgram = "/usr/bin/google-chrome"
const BrowserMaximumStartTime = time.Minute

// BrowserRequest 只授权一个用户级 Chrome 实例；不接受 Bot 身份、网址、命令、代理或 profile 覆盖。
// Deadline 约束迟到启动，已启动服务的寿命由监督器和独立资源组管理。
type BrowserRequest struct {
	Version  int                           `json:"version"`
	Group    processgroup.ServiceReference `json:"group"`
	Deadline agentchannel.Deadline         `json:"deadline"`
}

func (request BrowserRequest) Validate() error {
	if request.Version != 1 || request.Group.Validate() != nil || request.Group.Kind != processgroup.BrowserService || request.Deadline.ExpiresNS <= 0 || request.Deadline.TimeNamespace == 0 {
		return errors.New("共享浏览器启动授权无效")
	}
	return nil
}

// RuntimeDirectory 仅在 Validate 成功后使用，每个不可复用的服务实例有独立临时目录。
func (request BrowserRequest) RuntimeDirectory() string {
	return filepath.Join(computer.BrowserRuntimeRoot, request.Group.ID)
}

func encodeBrowser(request BrowserRequest) ([]byte, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > 16<<10 {
		return nil, errors.New("共享浏览器启动授权超过限制")
	}
	return encoded, nil
}

// DecodeBrowser 拒绝重复字段、未知覆盖和非规范编码，不能把其他服务授权当作浏览器授权。
func DecodeBrowser(encoded []byte) (BrowserRequest, error) {
	var request BrowserRequest
	if len(encoded) == 0 || len(encoded) > 16<<10 || json.Unmarshal(encoded, &request) != nil {
		return request, errors.New("共享浏览器启动授权格式无效")
	}
	canonical, err := encodeBrowser(request)
	if err != nil || !bytes.Equal(encoded, canonical) {
		return BrowserRequest{}, errors.New("共享浏览器启动授权不是规范编码")
	}
	return request, nil
}

func browserCommand(request BrowserRequest) ([]string, []string, error) {
	if err := request.Validate(); err != nil {
		return nil, nil, err
	}
	// Chrome 默认绕过环回代理，必须显式取消；无网络容器仍是直连的最终边界。
	arguments := []string{BrowserProgram, "--headless", "--no-first-run", "--no-default-browser-check",
		"--disable-background-networking", "--disable-component-update", "--disable-gpu", "--disable-quic",
		"--remote-debugging-pipe", "--proxy-server=http://127.0.0.1:3128", "--proxy-bypass-list=<-loopback>",
		"--user-data-dir=" + computer.BrowserDirectory, "about:blank"}
	environment := []string{"PATH=/usr/bin:/bin", "LANG=zh_CN.UTF-8", "HOME=" + computer.BrowserDirectory,
		"XDG_RUNTIME_DIR=" + request.RuntimeDirectory(), "TMPDIR=" + request.RuntimeDirectory()}
	return arguments, environment, nil
}
