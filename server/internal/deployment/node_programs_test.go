package deployment

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"io.yueyang/tom/internal/dockerbootstrap"
	"io.yueyang/tom/internal/nodeenrollment"
	"io.yueyang/tom/internal/noderelease"
)

// writeNodePrograms 用完整、同架构 ELF 头构造包，允许单独更换 Bot 或基础程序。
func writeNodePrograms(t *testing.T, i *nodeInstaller, dir string, node []byte, bot, core string) {
	t.Helper()
	profile := encodeFixture(t, noderelease.ProgramProfile{Version: 2, Label: "test", Platform: i.platform})
	files := map[string][]byte{"tomnode": node, "program-release.json": profile}
	for _, name := range noderelease.ComputerPrograms {
		content := append([]byte(nil), node...)
		if name == "bot-agent" {
			content = append(content, bot...)
		} else {
			content = append(content, core...)
		}
		files[name] = content
	}
	var sums strings.Builder
	for name, body := range files {
		writeInstallFixture(t, i.fs, filepath.Join(dir, name), body, 0755)
		sums.WriteString(contentDigest(body) + "  " + name + "\n")
	}
	writeInstallFixture(t, i.fs, filepath.Join(dir, noderelease.ProgramSums), []byte(sums.String()), 0644)
}

func TestUpgradeRejectsLegacyChecksumsNextToSplitPrograms(t *testing.T) {
	i, bin, _ := legacyRepairFixture(t)
	writeFullUpgradeFixture(t, i, []byte("旧节点程序"), []byte("旧镜像"), "example.test/base@sha256:"+strings.Repeat("a", 64))
	writeNodePrograms(t, i, "/source", bin, "bot", "core")
	_, err := i.repair(t.Context(), "/source/tomnode", NodeRepairOptions{Upgrade: true}, stoppedNode)
	if err == nil || !strings.Contains(err.Error(), "旧版完整包") || !strings.Contains(err.Error(), "独立电脑镜像包") {
		t.Fatalf("混合目录应提示拆包迁移方法，得到 %v", err)
	}
	if _, err := os.Lstat(i.fs.resolve(noderelease.ProgramRoot)); !os.IsNotExist(err) {
		t.Fatalf("混合目录仍开始写入电脑程序，得到 %v", err)
	}
}

func TestUpgradeRejectsPartialManifestsBeforeStaging(t *testing.T) {
	for _, name := range []string{noderelease.ProgramSums, "program-release.json", "SHA256SUMS", "node-release.json"} {
		t.Run(name, func(t *testing.T) {
			i, bin, _ := legacyRepairFixture(t)
			dir := "/programs"
			writeNodePrograms(t, i, dir, bin, "bot", "core")
			path := filepath.Join(dir, name)
			if name == noderelease.ProgramSums || name == "program-release.json" {
				if err := os.Remove(i.fs.resolve(path)); err != nil {
					t.Fatal(err)
				}
			} else {
				writeInstallFixture(t, i.fs, path, []byte("不完整的镜像包"), 0644)
			}
			_, err := i.repair(t.Context(), filepath.Join(dir, "tomnode"), NodeRepairOptions{Upgrade: true}, stoppedNode)
			if err == nil || !strings.Contains(err.Error(), "发布目录不完整") {
				t.Fatalf("缺少配套清单仍继续升级，得到 %v", err)
			}
			if _, err := os.Lstat(i.fs.resolve(noderelease.ProgramRoot)); !os.IsNotExist(err) {
				t.Fatal("检查完整性前已写入节点程序", err)
			}
		})
	}
}

func TestProgramUpgradeReusesImageAndRollsBackBotWithoutRestart(t *testing.T) {
	i, bin, _ := legacyRepairFixture(t)
	original, request := writeSharedBrowserFixture(t, i)
	// 首次升级的本地目录可同时接收两个归档；以后上传的小包完全独立。
	writeNodePrograms(t, i, "/programs", bin, "first", "core")
	ref := "example.test/base@sha256:" + strings.Repeat("a", 64)
	image := []byte("基础镜像")
	profile := encodeFixture(t, nodeRelease{Version: 2, Label: "base", Platform: i.platform, ImageReference: ref, ArchiveSHA256: contentDigest(image), Docker: encodeFixture(t, dockerbootstrap.DefaultManifest())})
	writeInstallFixture(t, i.fs, "/image/node-release.json", profile, 0644)
	writeInstallFixture(t, i.fs, "/image/computer-image.tar", image, 0644)
	writeInstallFixture(t, i.fs, "/image/SHA256SUMS", []byte(contentDigest(image)+"  computer-image.tar\n"+contentDigest(profile)+"  node-release.json\n"), 0644)
	i.inspectRecovery = func(context.Context, nodeInstallRequest, string) (*nodeenrollment.RecoveryInspection, error) {
		return &nodeenrollment.RecoveryInspection{State: "clear"}, nil
	}
	var stops, holds, commands int
	i.maintain = func(_ context.Context, _ nodeInstallRequest, _ string, end bool, _ string) error {
		if !end {
			holds++
		}
		return nil
	}
	i.systemctl = func(_ context.Context, args ...string) error {
		commands++
		if args[0] == "stop" {
			stops++
		}
		return nil
	}
	run := func(dir string) (NodeInstallationReport, error) {
		return i.repair(t.Context(), "/programs/tomnode", NodeRepairOptions{Upgrade: true, ImagePackage: dir}, stoppedNode)
	}
	if _, err := run(""); err == nil || stops != 0 {
		t.Fatal("首次切换缺镜像仍停服务", err)
	}
	writeNodePrograms(t, i, "/image", bin, "first", "core")
	initial, err := i.repair(t.Context(), "/image/tomnode", NodeRepairOptions{Upgrade: true}, stoppedNode)
	if err != nil || stops != 1 {
		t.Fatal(initial, err, stops)
	}
	checkSharedBrowserMigration(t, i, original, request.NodeID)
	if repeated, err := run("/image"); err != nil || repeated.ImageID != initial.ImageID || stops != 1 {
		t.Fatal("显式指定同一镜像目录应复用已安装的镜像", repeated, err)
	}
	active := func() string {
		t.Helper()
		target, err := os.Readlink(i.fs.resolve(noderelease.ProgramRoot + "/bot"))
		if err != nil {
			t.Fatal(err)
		}
		return target
	}
	first := active()
	if _, err := run("/missing-image"); err == nil || stops != 1 {
		t.Fatal("显式指定的镜像包缺失仍继续升级", err)
	}
	inspect := i.inspectRecovery
	i.inspectRecovery = func(context.Context, nodeInstallRequest, string) (*nodeenrollment.RecoveryInspection, error) {
		t.Fatal("Bot 热更新进入电脑验收等待")
		return nil, nil
	}
	systemCalls := commands
	for _, version := range []string{"second", "first"} {
		writeNodePrograms(t, i, "/programs", bin, version, "core")
		before := holds
		report, err := run("")
		if err != nil || commands != systemCalls || report.UpgradeScope != "bot" || holds != before || stops != 1 || report.ImageID != initial.ImageID || (active() == first) != (version == "first") {
			t.Fatal(version, report, err, holds, stops)
		}
	}
	i.inspectRecovery = inspect
	writeNodePrograms(t, i, "/programs", bin, "first", "changed core")
	if report, err := run(""); err != nil || report.ImageID != initial.ImageID || stops != 2 {
		t.Fatal(report, err, stops)
	}
	// 只有镜像包也可通过现有节点程序独立切换，重复下发相同镜像可以直接使用本地已核验副本。
	runtimeDigest, err := noderelease.RuntimeDigest(encodeFixture(t, dockerbootstrap.DefaultManifest()))
	if err != nil {
		t.Fatal(err)
	}
	manifest := noderelease.Manifest{ImageReference: ref, ArchiveSHA256: contentDigest(image), RuntimeSHA256: runtimeDigest}
	installed, err := i.installedReleaseImage(t.Context(), manifest)
	if err != nil || !installed {
		t.Fatal("已安装镜像未复用", err)
	}
	if _, err := i.repair(t.Context(), nodeBinaryPath, NodeRepairOptions{Upgrade: true, ImagePackage: "/image"}, stoppedNode); err != nil || stops != 2 {
		t.Fatal("相同镜像重复停机", err, stops)
	}
	writeNodePrograms(t, i, "/programs", bin, "third", "changed core")
	writeInstallFixture(t, i.fs, "/programs/bot-agent", []byte("损坏"), 0755)
	if _, err := run(""); err == nil || stops != 2 || active() != first {
		t.Fatal("损坏程序进入运行目录", err, stops)
	}
	nextImage := []byte("新版桌面环境")
	nextReference := "example.test/base@sha256:" + strings.Repeat("b", 64)
	nextProfile := encodeFixture(t, nodeRelease{Version: 2, Label: "base-next", Platform: i.platform, ImageReference: nextReference, ArchiveSHA256: contentDigest(nextImage), Docker: encodeFixture(t, dockerbootstrap.DefaultManifest())})
	writeInstallFixture(t, i.fs, "/image/node-release.json", nextProfile, 0644)
	writeInstallFixture(t, i.fs, "/image/computer-image.tar", nextImage, 0644)
	writeInstallFixture(t, i.fs, "/image/SHA256SUMS", []byte(contentDigest(nextImage)+"  computer-image.tar\n"+contentDigest(nextProfile)+"  node-release.json\n"), 0644)
	updated, err := i.repair(t.Context(), nodeBinaryPath, NodeRepairOptions{Upgrade: true, ImagePackage: "/image"}, stoppedNode)
	if err != nil || updated.ImageID == initial.ImageID || updated.NodeID != initial.NodeID || active() != first || stops != 3 {
		t.Fatal("独立镜像升级未保留程序与节点身份", updated, err, stops)
	}
	if installed, err := i.installedReleaseImage(t.Context(), manifest); err != nil || installed {
		t.Fatal("镜像切换后仍报告旧镜像已安装", err)
	}
}

func TestAutomaticProgramInstallationChecksBothPackagesAndSavedBinaries(t *testing.T) {
	i, reads, _ := automaticFixture(t)
	read := func(path string) []byte {
		t.Helper()
		content, err := os.ReadFile(i.fs.resolve(path))
		if err != nil {
			t.Fatal(err)
		}
		return content
	}
	var profile nodeRelease
	if err := json.Unmarshal(read("/source/node-release.json"), &profile); err != nil {
		t.Fatal(err)
	}
	writeNodePrograms(t, i, "/source", read("/source/tomnode"), "bot", "core")
	install := func() error { _, err := i.install(t.Context(), "", "/source/tomnode"); return err }
	if err := install(); err == nil || !strings.Contains(err.Error(), "旧版完整包") || *reads != 0 {
		t.Fatal("混入旧镜像的拆分程序包仍进入首次接入", err)
	}
	profile.Version, profile.Label = 2, "desktop"
	body := encodeFixture(t, profile)
	writeInstallFixture(t, i.fs, "/source/node-release.json", body, 0644)
	if err := install(); err == nil || *reads != 0 {
		t.Fatal("镜像缺少校验清单仍接入节点", err)
	}
	writeInstallFixture(t, i.fs, "/source/SHA256SUMS", []byte(profile.ArchiveSHA256+"  computer-image.tar\n"+contentDigest(body)+"  node-release.json\n"), 0644)
	if err := install(); err != nil {
		t.Fatal(err)
	}
	record, err := i.installedNodeRecord()
	if err != nil {
		t.Fatal(err)
	}
	request, _, err := parseNodeInstallRequest(record.Request)
	if err != nil || !request.Programs.valid() {
		t.Fatal(request.Programs, err)
	}
	binary := filepath.Join(noderelease.ProgramRoot, request.Programs.Core, "computer-task")
	if err := os.WriteFile(i.fs.resolve(binary), []byte("已损坏"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := install(); err == nil || *reads != 1 {
		t.Fatal("重试接受了已损坏的基础代理或重新申请身份", err)
	}
}
