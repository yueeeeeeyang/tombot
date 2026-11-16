// Package linuxhost 按发行版选择宿主安装方式，实际运行条件由各服务检查。
package linuxhost

import (
	"errors"
	"regexp"
	"strings"
)

// Release 包含软件仓库所需的公开发行信息，不把版本号作为允许安装的白名单。
type Release struct {
	ID                 string
	Family             string
	Version            string
	Suite              string
	DockerDistribution string
}

// ParseRelease 解析 os-release，不执行 Shell；未知发行家族或会扩大仓库路径的值返回错误。
func ParseRelease(encoded []byte) (Release, error) {
	var release Release
	if len(encoded) == 0 || len(encoded) > 64<<10 {
		return release, errors.New("Linux 发行信息大小无效")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(encoded), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			return release, errors.New("Linux 发行信息格式无效")
		}
		if _, present := fields[name]; present {
			return release, errors.New("Linux 发行信息包含重复字段")
		}
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
		fields[name] = value
	}
	release.ID, release.Version, release.Suite = fields["ID"], fields["VERSION_ID"], fields["VERSION_CODENAME"]
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`).MatchString(release.ID) || !regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)*$`).MatchString(release.Version) {
		return release, errors.New("Linux 发行标识或版本格式无效")
	}
	switch release.ID {
	case "ubuntu", "debian":
		release.Family, release.DockerDistribution = "apt", release.ID
	case "fedora", "rhel", "centos":
		release.Family, release.DockerDistribution = "dnf", release.ID
	case "rocky", "almalinux":
		release.Family, release.DockerDistribution = "dnf", "rhel"
	default:
		return release, errors.New("自动安装支持 Debian、Ubuntu、RHEL、Rocky、AlmaLinux、CentOS Stream 和 Fedora；未识别的发行版不能自动配置软件源")
	}
	if release.Family == "apt" && !regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`).MatchString(release.Suite) {
		return release, errors.New("APT 发行版缺少有效仓库代号")
	}
	return release, nil
}

func (release Release) Manager() string {
	if release.Family == "apt" {
		return "/usr/bin/apt-get"
	}
	return "/usr/bin/dnf"
}

func (release Release) PackageArchitecture(architecture string) string {
	if release.Family == "dnf" {
		if architecture == "amd64" {
			return "x86_64"
		}
		if architecture == "arm64" {
			return "aarch64"
		}
	}
	return architecture
}

// DockerKeyFingerprint 固定 Docker 官方仓库信任根，拒绝仅因下载成功而信任新公钥。
func (release Release) DockerKeyFingerprint() string {
	if release.Family == "apt" {
		return "9DC858229FC7DD38854AE2D88D81803C0EBFCD88"
	}
	return "060A61C51B558A7F742B77AAC52FEB6B621E9F35"
}

func (release Release) DockerKeyURL() string {
	return "https://download.docker.com/linux/" + release.DockerDistribution + "/gpg"
}

func (release Release) DockerSource(architecture string) string {
	if release.Family == "apt" {
		return "Types: deb\nURIs: https://download.docker.com/linux/" + release.DockerDistribution + "\nSuites: " + release.Suite + "\nComponents: stable\nArchitectures: " + architecture + "\nSigned-By: /etc/apt/keyrings/tombot-docker.asc\n"
	}
	version := release.Version
	if release.DockerDistribution != "fedora" {
		version, _, _ = strings.Cut(version, ".")
	}
	return "[tombot-docker]\nname=TomBot Docker CE stable\nbaseurl=https://download.docker.com/linux/" + release.DockerDistribution + "/" + version + "/" + release.PackageArchitecture(architecture) + "/stable\nenabled=1\ngpgcheck=1\nsslverify=1\ngpgkey=file:///etc/pki/rpm-gpg/tombot-docker.asc\n"
}
