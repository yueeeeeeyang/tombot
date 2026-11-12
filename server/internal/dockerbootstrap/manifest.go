// Package dockerbootstrap 为受保护的 Linux 启动引导安装并复验 Docker，不提供远程命令接口。
package dockerbootstrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
)

// Manifest 由发布包写入 root 保护的配置文件，版本和仓库签名指纹必须经过部署验证。
type Manifest struct {
	EnginePackageVersion     string   `json:"engine_package_version"`
	CLIPackageVersion        string   `json:"cli_package_version"`
	ContainerdPackageVersion string   `json:"containerd_package_version"`
	E2fsprogsPackageVersion  string   `json:"e2fsprogs_package_version"`
	UtilLinuxPackageVersion  string   `json:"util_linux_package_version"`
	MountPackageVersion      string   `json:"mount_package_version"`
	AllowedEngineVersions    []string `json:"allowed_engine_versions"`
	RepositoryFingerprint    string   `json:"repository_fingerprint"`
	TimeoutSeconds           int      `json:"timeout_seconds"`
	MinimumFreeBytes         uint64   `json:"minimum_free_bytes"`
}

// ParseManifest 校验明确版本、签名及预算，不探测或安装宿主软件。
func ParseManifest(encoded []byte) (Manifest, error) {
	var manifest Manifest
	if len(encoded) == 0 || len(encoded) > 64<<10 {
		return manifest, errors.New("Docker 安装清单大小无效")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, errors.New("Docker 安装清单格式无效")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return manifest, errors.New("Docker 安装清单包含多份配置")
	}
	version := regexp.MustCompile(`^[0-9][0-9A-Za-z.+:~_-]{0,100}$`)
	for _, value := range []string{manifest.EnginePackageVersion, manifest.CLIPackageVersion, manifest.ContainerdPackageVersion, manifest.E2fsprogsPackageVersion, manifest.UtilLinuxPackageVersion, manifest.MountPackageVersion} {
		if !version.MatchString(value) {
			return manifest, errors.New("Docker 和宿主磁盘工具必须指定准确的软件包版本")
		}
	}
	if len(manifest.AllowedEngineVersions) < 1 || len(manifest.AllowedEngineVersions) > 10 || manifest.TimeoutSeconds < 30 || manifest.TimeoutSeconds > 3600 || manifest.MinimumFreeBytes < 1<<30 || manifest.MinimumFreeBytes > 100<<30 || !regexp.MustCompile(`^[0-9A-F]{40}$`).MatchString(manifest.RepositoryFingerprint) {
		return manifest, errors.New("Docker 兼容版本、签名指纹、磁盘余量或安装时限无效")
	}
	for _, value := range manifest.AllowedEngineVersions {
		if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(value) {
			return manifest, errors.New("Docker 兼容版本必须精确到补丁版本")
		}
	}
	matched := false
	for _, value := range manifest.AllowedEngineVersions {
		if strings.HasPrefix(manifest.EnginePackageVersion, "5:"+value+"-") {
			matched = true
		}
	}
	if !matched || manifest.CLIPackageVersion != manifest.EnginePackageVersion || !strings.HasSuffix(manifest.EnginePackageVersion, "~ubuntu.24.04~noble") {
		return manifest, errors.New("Docker 安装版本必须属于本发行版的受测范围，且引擎与客户端版本一致")
	}
	return manifest, nil
}

func (manifest Manifest) allows(version string) bool {
	for _, allowed := range manifest.AllowedEngineVersions {
		if version == allowed {
			return true
		}
	}
	return false
}

func ubuntuRelease(encoded []byte) error {
	fields := make(map[string]string)
	for _, line := range strings.Split(string(encoded), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			return errors.New("操作系统发行信息无效")
		}
		if _, exists := fields[name]; exists {
			return errors.New("操作系统发行信息包含重复字段")
		}
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
		fields[name] = value
	}
	if fields["ID"] != "ubuntu" || fields["VERSION_ID"] != "24.04" || fields["VERSION_CODENAME"] != "noble" {
		return errors.New("当前自动引导仅支持 Ubuntu 24.04 LTS，其他发行版尚未验收")
	}
	return nil
}

func repositorySource(architecture string) string {
	return "Types: deb\nURIs: https://download.docker.com/linux/ubuntu\nSuites: noble\nComponents: stable\nArchitectures: " + architecture + "\nSigned-By: /etc/apt/keyrings/tombot-docker.asc\n"
}
