package computer

import (
	_ "embed"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const DesktopRuntimeRoot = "/run/bot-desktops"

// 同一份模板嵌入任务引导并打包给 XFCE 启动器，避免两种入口的共享工具和桌面路由发生漂移。
//
//go:embed desktop-environment.json
var desktopEnvironmentTemplate string

// EgressAddress 从与 XFCE 共用的权威环境模板读取转接端点，拒绝非环回代理或携带认证的 URL。
func EgressAddress() (string, error) {
	var environment map[string]string
	if json.Unmarshal([]byte(desktopEnvironmentTemplate), &environment) != nil {
		return "", errors.New("电脑环境模板无效")
	}
	parsed, err := url.Parse(environment["http_proxy"])
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || !net.ParseIP(parsed.Hostname()).Equal(net.IPv4(127, 0, 0, 1)) {
		return "", errors.New("电脑环回代理模板无效")
	}
	port, err := strconv.ParseUint(parsed.Port(), 10, 16)
	if err != nil || port < 1024 {
		return "", errors.New("电脑环回代理端口无效")
	}
	for _, name := range []string{"https_proxy", "HTTP_PROXY", "HTTPS_PROXY"} {
		if environment[name] != environment["http_proxy"] {
			return "", errors.New("电脑代理环境来源不一致")
		}
	}
	return parsed.Host, nil
}

// RuntimeDirectory 按不可复用的执行会话生成私有图形状态路径，不作为业务文件目录。
// 调用方必须先校验完整执行身份，旧会话目录不能被下一次授权直接复用。
func (session ExecutionSession) RuntimeDirectory() string {
	return DesktopRuntimeRoot + "/" + session.SessionID
}

// DesktopEnvironment 为已校验会话生成独立映射，不继承宿主环境，不在进程间共享可变配置。
func (session ExecutionSession) DesktopEnvironment() (map[string]string, error) {
	if err := session.Validate(); err != nil {
		return nil, err
	}
	var environment map[string]string
	if err := json.Unmarshal([]byte(desktopEnvironmentTemplate), &environment); err != nil {
		return nil, errors.New("电脑桌面环境模板无效")
	}
	replacer := strings.NewReplacer("{{uid}}", strconv.Itoa(session.UID), "{{display}}", strconv.Itoa(session.DisplayNumber), "{{runtime}}", session.RuntimeDirectory())
	for name, value := range environment {
		value = replacer.Replace(value)
		if strings.Contains(value, "{{") {
			return nil, errors.New("电脑桌面环境包含未知占位符")
		}
		environment[name] = value
	}
	return environment, nil
}
