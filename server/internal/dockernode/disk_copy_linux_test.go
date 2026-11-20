//go:build diskintegration

package dockernode

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

// TestOfflineCopyPreservesExt4 通过真实 ext4 工具验收目标副本，不要求宿主挂载或访问 Docker 套接字。
func TestOfflineCopyPreservesExt4(t *testing.T) {
	exerciseOfflineCopy(t, "")
}

// TestOfflineCopyPreservesBrowserLogin 在源桌面实际登录本地站点，再用目标盘导出的资料启动独立浏览器。
func TestOfflineCopyPreservesBrowserLogin(t *testing.T) {
	browser, err := exec.LookPath("chromium")
	if err != nil {
		t.Skip("浏览器登录态验收要求安装 Chromium")
	}
	exerciseOfflineCopy(t, browser)
}

func exerciseOfflineCopy(t *testing.T, browser string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	source, target := newFixture(t), newFixture(t)
	from := source.specification
	from.Resources.DiskBytes = 520 << 20
	to := from
	to.NodeID = target.node.options.NodeID
	to.Generation++
	to.Attempt++
	to.FencingToken++
	to.Resources.DiskBytes = 512 << 20
	initial, err := source.node.Ensure(ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	q := computer.DiskCopy{EditID: ids.New(), Source: from, Target: to, Container: initial.ContainerID}
	for _, node := range []*Runtime{source.node, target.node} {
		fixture := newStorageFixture(t)
		spec := to
		if node == source.node {
			spec = from
		}
		fixture.resource = node.resource(spec)
		s := fixture.storage
		s.options.Directory = node.options.StorageDirectory
		host, err := storageCommands(45 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		// 容器引擎、挂载和 loop 使用节点测试后端；格式化、缩容、UUID 和内容复核使用真实 ext4。
		s.run = func(ctx context.Context, program string, args ...string) (storageResult, error) {
			switch program {
			case "mount", "umount", "losetup":
				return fixture.command(ctx, program, args...)
			}
			return host(ctx, program, args...)
		}
		if err = os.Mkdir(fixture.backing, 0755); err != nil {
			t.Fatal(err)
		}
		if err = s.layout.prepare(fixture.backing, true, true); err != nil {
			t.Fatal(err)
		}
		node.storage = s
	}
	s := source.node.storage.(*FileStorage)
	res := source.node.resource(from)
	if err := s.checkDirectory(res, true); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	files := []string{"workspace/shared.txt", "desktops/" + ids.New() + "/Default/Cookies", "browser/old-profile"}
	for _, file := range files {
		path := filepath.Join(root, file)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var verifyLogin func(string)
	if browser != "" {
		desktopID := ids.New()
		profile := filepath.Join(root, "desktops", desktopID)
		if err = os.Mkdir(profile, 0700); err != nil {
			t.Fatal(err)
		}
		session := ids.New()
		site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/signin" {
				http.SetCookie(w, &http.Cookie{Name: "session", Value: session, Path: "/", MaxAge: 3600, HttpOnly: true})
			} else if cookie, err := r.Cookie("session"); err != nil || cookie.Value != session {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if _, err := w.Write([]byte("<p>session verified</p>")); err != nil {
				t.Error(err)
			}
		}))
		defer site.Close()
		visit := func(profile, path string) {
			t.Helper()
			cmd := exec.CommandContext(ctx, browser, "--headless", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage", "--no-first-run", "--user-data-dir="+profile, "--dump-dom", site.URL+path)
			cmd.Env = append(os.Environ(), "HOME="+profile, "XDG_CONFIG_HOME="+profile+"/.config")
			out, err := cmd.Output()
			if err != nil || !bytes.Contains(out, []byte("session verified")) {
				t.Fatal("测试站点未确认浏览器会话", err)
			}
		}
		visit(profile, "/signin")
		verifyLogin = func(diskPath string) {
			destination := t.TempDir()
			if err := exec.CommandContext(ctx, "/usr/sbin/debugfs", "-R", "rdump /desktops/"+desktopID+" "+destination, diskPath).Run(); err != nil {
				t.Fatal(err)
			}
			visit(filepath.Join(destination, desktopID), "/account")
		}
	}
	f, err := s.openImage(res, true)
	if err != nil {
		t.Fatal(err)
	}
	disk := initialStorageRecord(res)
	disk.ImageDevice, disk.ImageInode, _, _, err = imageIdentity(f)
	if err = errors.Join(err, f.Truncate(from.Resources.DiskBytes), f.Close()); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, "/usr/sbin/mkfs.ext4", "-q", "-F", "-d", root, "-U", disk.UUID, s.image(res)).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	disk.Phase = "ready"
	disk.DesktopProfiles = true
	if err = s.save(disk); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Restore(ctx, res); err != nil {
		t.Fatal(err)
	}
	call := func(n *Runtime, action string) computer.DiskReceipt {
		t.Helper()
		q.Action = action
		q.Target.LeaseExpiresAt = time.Now().Add(time.Minute)
		v, err := n.CopyDisk(ctx, q)
		if err != nil || !v.Matches(q) {
			var failure *computer.Failure
			if errors.As(err, &failure) {
				t.Fatalf("%s：%s", action, failure.Code)
			}
			t.Fatalf("%s：%v", action, err)
		}
		return v
	}
	call(target.node, "preflight")
	call(target.node, "preflight")
	call(source.node, "freeze")
	call(source.node, "freeze")
	if len(source.engine.containers) != 0 {
		t.Fatal("原实例仍在运行")
	}
	for q.Offset < from.Resources.DiskBytes {
		q.Chunk = nil
		read := call(source.node, "read")
		q.Chunk = read.Chunk
		write := call(target.node, "write")
		if q.Offset == 0 {
			call(target.node, "write")
		}
		q.Offset = write.Offset
	}
	q.Chunk = nil
	q.Offset = 0
	p, err := target.node.loadCopy(q)
	if err != nil || p.Copied != from.Resources.DiskBytes {
		t.Fatal("复制游标不完整", err)
	}
	dest := target.node.storage.(*FileStorage)
	r := target.node.resource(to)
	// 安装回执丢失后，以新租约重试同一目标，只能产生一个通过探针的新实例。
	installed := call(target.node, "install")
	q.Target.FencingToken++
	again := call(target.node, "install")
	if installed.Observation.ContainerID != again.Observation.ContainerID || len(target.engine.containers) != 1 {
		t.Fatal("重复启用了目标电脑")
	}
	for _, path := range []string{s.image(res), dest.image(r)} {
		for _, file := range files {
			out, err := exec.CommandContext(ctx, "/usr/sbin/debugfs", "-R", "cat "+file, path).Output()
			if err != nil || !bytes.Equal(out, []byte(file)) {
				t.Fatal("盘中资料不一致", file, err)
			}
		}
	}
	if verifyLogin != nil {
		verifyLogin(dest.image(r))
	}
	stat, err := os.Stat(dest.image(r))
	if err != nil || stat.Size() != to.Resources.DiskBytes {
		t.Fatal("目标容量未生效", err)
	}
}
