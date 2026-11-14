// Package deploy 保存随发布包交付的配置基线，避免生成器与部署示例各自维护默认值。
package deploy

import _ "embed"

// NodeDirectory 是安装器管理的节点配置目录，与 API 文件分开保存。
const NodeDirectory = "/etc/tombot/node"

//go:embed node.example.json
var nodeExample []byte

//go:embed systemd/tombot-node.service
var nodeService string

//go:embed tomd.service
var serverService string

// ServerService 返回与服务端程序一同发布的系统服务单元。
func ServerService() string { return serverService }

//go:embed node-install.example.json
var nodeInstallTemplate string

// NodeExample 返回独立副本，调用者修改不会影响后续生成。
func NodeExample() []byte { return append([]byte(nil), nodeExample...) }

// NodeService 返回随二进制发布的节点服务单元，部署无需另行下载模板。
func NodeService() string { return nodeService }

// NodeInstallTemplate 返回待填写的安装模板；环境身份与受测版本必须由部署者提供。
func NodeInstallTemplate() string { return nodeInstallTemplate }
