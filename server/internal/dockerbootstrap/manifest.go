// Package dockerbootstrap 为受保护的 Linux 启动引导安装并复验 Docker，不提供远程命令接口。
package dockerbootstrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Manifest 只限定安装预算；软件包由宿主签名仓库解析，运行时按能力验收。
type Manifest struct {
	Version          int    `json:"version"`
	TimeoutSeconds   int    `json:"timeout_seconds"`
	MinimumFreeBytes uint64 `json:"minimum_free_bytes"`
}

// DefaultManifest 返回跨发行版安装预算，不携带系统补丁版本。
func DefaultManifest() Manifest {
	return Manifest{Version: 2, TimeoutSeconds: 3600, MinimumFreeBytes: 1 << 30}
}

// ParseManifest 拒绝旧版本锁定字段、未知字段与无界预算，不探测或修改宿主。
func ParseManifest(encoded []byte) (Manifest, error) {
	var manifest Manifest
	if len(encoded) == 0 || len(encoded) > 64<<10 {
		return manifest, errors.New("Docker 安装清单大小无效")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, errors.New("Docker 安装清单格式无效；旧安装请使用 tomnode repair 迁移")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return manifest, errors.New("Docker 安装清单包含多份配置")
	}
	if manifest.Version != 2 || manifest.TimeoutSeconds < 30 || manifest.TimeoutSeconds > 3600 || manifest.MinimumFreeBytes < 1<<30 || manifest.MinimumFreeBytes > 100<<30 {
		return manifest, errors.New("Docker 安装清单版本、磁盘余量或安装时限无效")
	}
	return manifest, nil
}
