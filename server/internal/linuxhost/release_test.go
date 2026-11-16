package linuxhost

import (
	"strings"
	"testing"
)

func TestReleaseFamiliesWithoutVersionWhitelist(t *testing.T) {
	for _, test := range []struct{ id, version, suite, family, distribution string }{
		{"ubuntu", "22.04", "jammy", "apt", "ubuntu"}, {"ubuntu", "26.04", "resolute", "apt", "ubuntu"},
		{"debian", "12", "bookworm", "apt", "debian"}, {"debian", "13", "trixie", "apt", "debian"},
		{"rhel", "9.7", "", "dnf", "rhel"}, {"rocky", "10.0", "", "dnf", "rhel"},
		{"almalinux", "9.6", "", "dnf", "rhel"}, {"centos", "10", "", "dnf", "centos"}, {"fedora", "44", "", "dnf", "fedora"},
	} {
		t.Run(test.id+test.version, func(t *testing.T) {
			release, err := ParseRelease([]byte("ID=" + test.id + "\nVERSION_ID=\"" + test.version + "\"\nVERSION_CODENAME=" + test.suite + "\n"))
			if err != nil || release.Family != test.family || release.DockerDistribution != test.distribution {
				t.Fatal(release, err)
			}
			source := release.DockerSource("amd64")
			if !strings.Contains(source, "https://download.docker.com/linux/"+test.distribution) || strings.Contains(source, "noble") {
				t.Fatal("仓库没有匹配实际发行信息", source)
			}
			if test.family == "dnf" && (!strings.Contains(source, "gpgcheck=1") || !strings.Contains(source, "sslverify=1") || !strings.Contains(source, "/x86_64/")) {
				t.Fatal("RPM 软件源缺少签名、TLS 或架构限制")
			}
		})
	}
}

func TestReleaseRejectsMalformedSources(t *testing.T) {
	for _, input := range []string{"", "ID=ubuntu\nID=debian\nVERSION_ID=24.04\nVERSION_CODENAME=noble", "ID=alpine\nVERSION_ID=3.20", "ID=ubuntu\nVERSION_ID=24.04\nVERSION_CODENAME=../../other", "ID=rhel\nVERSION_ID=9/$basearch", "ID=debian\nVERSION_ID=13"} {
		if _, err := ParseRelease([]byte(input)); err == nil {
			t.Fatal("非法发行信息被用作软件源", input)
		}
	}
}
