package deployment

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/dockerbootstrap"
)

func TestNodeRepairUpgradesOfflineImageWithoutReplacingIdentity(t *testing.T) {
	for _, scenario := range []string{"success", "corrupt", "platform"} {
		t.Run(scenario, func(t *testing.T) {
			installer, _, protected := legacyRepairFixture(t)
			beforeBytes, err := os.ReadFile(installer.fs.resolve(deploy.NodeDirectory + "/installation.json"))
			if err != nil {
				t.Fatal(err)
			}
			var before nodeInstallation
			if err = json.Unmarshal(beforeBytes, &before); err != nil {
				t.Fatal(err)
			}
			old, _, err := parseNodeInstallRequest(before.Request)
			if err != nil {
				t.Fatal(err)
			}
			archive := []byte("新的离线测试归档")
			release := nodeRelease{Version: 1, Platform: installer.platform, ImageReference: "registry.example.test/computer@sha256:" + strings.Repeat("d", 64), ArchiveSHA256: contentDigest(archive), Docker: encodeFixture(t, dockerbootstrap.DefaultManifest())}
			if scenario == "platform" {
				release.Platform = "linux/invalid"
			}
			if scenario == "corrupt" {
				archive = append(archive, 1)
			}
			writeInstallFixture(t, installer.fs, "/source/node-release.json", encodeFixture(t, release), 0644)
			writeInstallFixture(t, installer.fs, "/source/computer-image.tar", archive, 0644)
			report, err := installer.repair(t.Context(), "/source/tomnode", NodeRepairOptions{ImagePackage: "/source"}, stoppedNode)
			if scenario != "success" {
				if err == nil {
					t.Fatal("无效镜像被接受")
				}
				actual, readErr := os.ReadFile(installer.fs.resolve(deploy.NodeDirectory + "/installation.json"))
				if readErr != nil || !bytes.Equal(actual, beforeBytes) {
					t.Fatal("失败升级改变了安装状态", readErr)
				}
				return
			}
			if err != nil || report.NodeID != old.NodeID || report.ImageID == old.ImageID {
				t.Fatal(report, err)
			}
			encoded, err := os.ReadFile(installer.fs.resolve(deploy.NodeDirectory + "/installation.json"))
			if err != nil {
				t.Fatal(err)
			}
			var after nodeInstallation
			if err = json.Unmarshal(encoded, &after); err != nil {
				t.Fatal(err)
			}
			request, _, err := parseNodeInstallRequest(after.Request)
			if err != nil || request.PreviousImageID != old.ImageID || request.Image.Reference != release.ImageReference {
				t.Fatal(request, err)
			}
			if _, err = installer.verifyInstallation(t.Context(), after); err != nil {
				t.Fatal(err)
			}
			for name, want := range protected {
				actual, err := os.ReadFile(installer.fs.resolve(deploy.NodeDirectory + "/" + name))
				if err != nil || !bytes.Equal(actual, want) {
					t.Fatal("身份或旧归档被覆盖", name, err)
				}
			}
		})
	}
}
