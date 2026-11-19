package deployment

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"io.yueyang/tom/internal/dockerbootstrap"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodeenrollment"
	"io.yueyang/tom/internal/noderelease"
)

func TestUpgradeStopsOnlyAfterValidationAndResumesWithoutStopping(t *testing.T) {
	i, bin, _ := legacyRepairFixture(t)
	image := []byte("升级包归档")
	writeFullUpgradeFixture(t, i, bin, image, "registry.example.test/bot@sha256:"+strings.Repeat("a", 64))
	i.inspectRecovery = func(context.Context, nodeInstallRequest, string) (*nodeenrollment.RecoveryInspection, error) {
		return &nodeenrollment.RecoveryInspection{State: "clear"}, nil
	}
	var sequence []string
	i.systemctl = func(ctx context.Context, args ...string) error { sequence = append(sequence, args[0]); return nil }
	held, finish := false, false
	i.maintain = func(_ context.Context, _ nodeInstallRequest, _ string, end bool) error {
		if end {
			sequence = append(sequence, "finish")
			if !finish {
				return errors.New("回执中断")
			}
		} else {
			sequence = append(sequence, "begin")
			if !held {
				return nodeenrollment.ErrMaintenanceConflict
			}
		}
		return nil
	}
	run := func() error {
		_, err := i.repair(t.Context(), "/source/tomnode", NodeRepairOptions{Upgrade: true, ImagePackage: "/source"}, stoppedNode)
		return err
	}
	writeInstallFixture(t, i.fs, "/source/computer-image.tar", append(bytes.Clone(image), 0), 0644)
	if run() == nil || len(sequence) != 0 {
		t.Fatal("坏包导致停机", sequence)
	}
	writeInstallFixture(t, i.fs, "/source/computer-image.tar", image, 0644)
	if !errors.Is(run(), nodeenrollment.ErrMaintenanceConflict) || strings.Join(sequence, ",") != "begin" {
		t.Fatal("繁忙时中断节点", sequence)
	}
	held = true
	sequence = nil
	if run() == nil || !strings.HasPrefix(strings.Join(sequence, ","), "begin,stop,daemon-reload,enable,is-active,finish") {
		t.Fatal("升级阶段错误", sequence)
	}
	finish = true
	sequence = nil
	if err := run(); err != nil || strings.Contains(strings.Join(sequence, ","), "stop") {
		t.Fatal("重试时重复停机", sequence, err)
	}
	sequence = nil
	if err := run(); err != nil || strings.Contains(strings.Join(sequence, ","), "stop") {
		t.Fatal("相同包重复停机", sequence, err)
	}
	if _, err := os.Stat(i.fs.resolve("/etc/systemd/system/tombot-node-update.timer")); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeDistinguishesBinaryBundleFromMismatchedProgram(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		files   []string
		program string
		want    string
	}{
		{"仅含宿主程序", []string{"tomnode", "tomd", "BUILD.txt", "README.zh-CN.md"}, "包内程序", "请解压完整节点离线包"},
		{"完整清单配错程序", []string{"tomnode", "node-release.json", "computer-image.tar", "node-install.example.json", "安装说明.txt"}, "另一个程序", "tomnode 与当前节点包的 SHA256SUMS 不匹配"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			i, _, _ := legacyRepairFixture(t)
			var sums strings.Builder
			for _, name := range scenario.files {
				sums.WriteString(contentDigest([]byte("包内程序")) + "  " + name + "\n")
			}
			writeInstallFixture(t, i.fs, "/source/SHA256SUMS", []byte(sums.String()), 0644)
			if _, err := i.checkUpgradePackage("/source", []byte(scenario.program)); err == nil || !strings.Contains(err.Error(), scenario.want) {
				t.Fatalf("节点包校验应提示%s，得到 %v", scenario.want, err)
			}
		})
	}
}

func TestUpgradeReceiptSurvivesLostReply(t *testing.T) {
	i, _, _ := legacyRepairFixture(t)
	if err := i.fs.ensureInstallDirectory(updateCache, 0700); err != nil {
		t.Fatal(err)
	}
	a := noderelease.Assignment{Job: ids.New(), Release: strings.Repeat("a", 64)}
	if err := i.fs.recordReceipt(a, "started"); err != nil {
		t.Fatal(err)
	}
	if previous, err := i.fs.previousReceipt(a); err != nil || previous != "started" {
		t.Fatal(previous, err)
	}
	a.Release = strings.Repeat("b", 64)
	if _, err := i.fs.previousReceipt(a); err == nil {
		t.Fatal("重放不同目标的升级")
	}
	a.Job = ids.New()
	if previous, err := i.fs.previousReceipt(a); err != nil || previous != "" {
		t.Fatal(previous, err)
	}
}

func writeFullUpgradeFixture(t *testing.T, i *nodeInstaller, bin, image []byte, reference string) {
	t.Helper()
	r := nodeRelease{Version: 1, Platform: i.platform, ImageReference: reference, ArchiveSHA256: contentDigest(image), Docker: encodeFixture(t, dockerbootstrap.DefaultManifest())}
	var sums strings.Builder
	for name, body := range map[string][]byte{"node-release.json": encodeFixture(t, r), "computer-image.tar": image, "node-install.example.json": []byte("{}"), "安装说明.txt": []byte("测试说明"), "tomnode": bin} {
		if name != "tomnode" {
			writeInstallFixture(t, i.fs, "/source/"+name, body, 0644)
		}
		sums.WriteString(contentDigest(body) + "  " + name + "\n")
	}
	writeInstallFixture(t, i.fs, "/source/SHA256SUMS", []byte(sums.String()), 0644)
}
