package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/auth"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/secret"
	"io.yueyang/tom/internal/store"
	"io.yueyang/tom/internal/testkit"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type instance struct {
	data, workspace, archive, target string
	db                               *sql.DB
	key                              []byte
}

func newInstance(t *testing.T) instance {
	t.Helper()
	root := t.TempDir()
	f := instance{data: filepath.Join(root, "data"), workspace: filepath.Join(root, "workspace"), archive: filepath.Join(root, "snapshot.tgz"), target: filepath.Join(root, "recovered")}
	must(t, os.Mkdir(f.workspace, 0700))
	var err error
	f.db, err = store.Open(t.Context(), filepath.Join(f.data, "tom.db"))
	must(t, err)
	t.Cleanup(func() {
		if err := f.db.Close(); err != nil {
			t.Error(err)
		}
	})
	f.key, err = secure.LoadKey(f.data, "")
	must(t, err)
	t.Cleanup(func() { clear(f.key) })
	return f
}

func freshEnvironment(t *testing.T) string {
	t.Helper()
	key, err := secure.Random(32)
	must(t, err)
	defer clear(key)
	return base64.StdEncoding.EncodeToString(key)
}

func TestRestoreDeviceSecretWorkspaceAndSearch(t *testing.T) {
	for _, source := range []string{"file", "environment"} {
		t.Run(source, func(t *testing.T) {
			f := newInstance(t)
			env := ""
			key := f.key
			if source == "environment" {
				env = freshEnvironment(t)
				var err error
				key, err = secure.ReadKey("", env)
				must(t, err)
				defer clear(key)
			}
			ctx := t.Context()
			authentication, err := auth.New(f.db, f.data)
			must(t, err)
			code, err := authentication.IssueSetupCode()
			must(t, err)
			password := testkit.Random(t)
			must(t, authentication.Setup(password, code))
			session, err := authentication.Login(ctx, password, "还原验收", "macos", nil)
			must(t, err)
			must(t, authentication.UpdatePushToken(ctx, session.DeviceID, testkit.Random(t), key))
			value := testkit.Random(t)
			entry, err := secret.New(f.db, key).Create(ctx, "恢复验证", value, "user")
			must(t, err)
			_, err = f.db.Exec("INSERT INTO conversations(id,kind,title,created_at,updated_at) VALUES('c','direct','验收',0,0); INSERT INTO messages(id,conversation_id,seq,sender_kind,blocks_json,created_at) VALUES('m','c',1,'user','[{\"kind\":\"text\",\"text\":\"恢复中文检索\"}]',0)")
			must(t, err)
			dir := filepath.Join(f.workspace, "资料")
			must(t, os.Mkdir(dir, 0700))
			content := []byte("备份工作区\n")
			filename := filepath.Join(dir, "运行")
			must(t, os.WriteFile(filename, content, 0700))
			must(t, os.Chmod(filename, 0750))
			modified := time.Unix(1700000000, 123456000)
			must(t, os.Chtimes(filename, modified, modified))
			must(t, os.Symlink("资料/运行", filepath.Join(f.workspace, "入口")))
			profile := filepath.Join(f.data, "browser-profile")
			must(t, os.Mkdir(profile, 0700))
			// macOS Chromium 把运行版本号存成悬空链接，它是进程标记，正常停机后仍可能存在。
			must(t, os.Symlink("157.0", filepath.Join(profile, "RunningChromeVersion")))
			must(t, Create(ctx, f.data, f.workspace, f.archive, env))
			stat, err := os.Stat(f.archive)
			must(t, err)
			if stat.Mode().Perm() != 0600 {
				t.Fatal("归档权限过宽")
			}
			members := readArchive(t, f.archive)
			var meta metadata
			must(t, json.Unmarshal(members[0].body, &meta))
			if meta.KeySource != source || meta.Notice != KeyNotice(env) {
				t.Fatal("密钥来源说明不一致")
			}
			found := false
			for _, member := range members {
				if operational(member.header.Name) {
					t.Fatal("运行时临时文件进入归档")
				}
				if member.header.Name == "data/master.key" {
					found = true
					if !bytes.Equal(member.body, key) {
						t.Fatal("归档取错主密钥")
					}
				}
				if source == "environment" && (bytes.Contains(member.body, key) || bytes.Contains(member.body, f.key)) {
					t.Fatal("环境密钥或过期文件密钥进入归档")
				}
			}
			if found != (source == "file") {
				t.Fatal("文件密钥归档规则失效")
			}
			if err := Restore(ctx, f.archive, f.target, freshEnvironment(t)); err == nil {
				t.Fatal("接受了错误环境密钥")
			}
			if source == "environment" {
				if err := Restore(ctx, f.archive, f.target, ""); err == nil {
					t.Fatal("缺少原环境密钥时还原成功")
				}
			}
			must(t, Restore(ctx, f.archive, f.target, env))
			restored := filepath.Join(f.target, "data")
			db, err := store.Open(ctx, filepath.Join(restored, "tom.db"))
			must(t, err)
			defer func() { must(t, db.Close()) }()
			login, err := auth.New(db, restored)
			must(t, err)
			device, err := login.Authenticate(ctx, session.DeviceToken)
			must(t, err)
			if device != session.DeviceID {
				t.Fatal("原设备身份改变")
			}
			restoredKey, err := secure.ReadKey(restored, env)
			must(t, err)
			defer clear(restoredKey)
			must(t, secret.New(db, restoredKey).Use(ctx, entry.ID, "user", "", func(plain []byte) error {
				if string(plain) != value {
					return errors.New("解密内容错误")
				}
				return nil
			}))
			var count int
			must(t, db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH '中文检索'").Scan(&count))
			if count != 1 {
				t.Fatal("消息全文索引丢失")
			}
			link := filepath.Join(f.target, "workspace", "入口")
			target, err := os.Readlink(link)
			must(t, err)
			got, err := os.ReadFile(link)
			must(t, err)
			stat, err = os.Stat(link)
			must(t, err)
			if target != "资料/运行" || !bytes.Equal(content, got) || stat.Mode().Perm() != 0750 || !modified.Equal(stat.ModTime()) {
				t.Fatal("工作区内容、权限、时间或相对链接变化")
			}
		})
	}
}

type member struct {
	header tar.Header
	body   []byte
}

func readArchive(t *testing.T, filename string) []member {
	t.Helper()
	file, err := os.Open(filename)
	must(t, err)
	defer func() { must(t, file.Close()) }()
	gzipReader, err := gzip.NewReader(file)
	must(t, err)
	defer func() { must(t, gzipReader.Close()) }()
	r := tar.NewReader(gzipReader)
	var members []member
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			return members
		}
		must(t, err)
		body, err := io.ReadAll(r)
		must(t, err)
		members = append(members, member{*h, body})
	}
}

func encodeArchive(t *testing.T, members []member) []byte {
	t.Helper()
	var compressed bytes.Buffer
	g := gzip.NewWriter(&compressed)
	w := tar.NewWriter(g)
	for _, m := range members {
		must(t, w.WriteHeader(&m.header))
		_, err := w.Write(m.body)
		must(t, err)
	}
	must(t, w.Close())
	must(t, g.Close())
	return compressed.Bytes()
}

func TestRejectUnsafeOrDamagedArchives(t *testing.T) {
	f := newInstance(t)
	must(t, os.WriteFile(filepath.Join(f.workspace, "文档"), []byte("验收"), 0600))
	must(t, Create(t.Context(), f.data, f.workspace, f.archive, ""))
	original := readArchive(t, f.archive)
	index := -1
	for i, m := range original {
		if m.header.Name == "workspace/文档" {
			index = i
		}
	}
	if index < 0 {
		t.Fatal("缺少工作区文件")
	}
	alter := func(fn func(*member)) func([]member) []member {
		return func(m []member) []member { fn(&m[index]); return m }
	}
	tests := map[string]func([]member) []member{
		"目录逃逸":  alter(func(m *member) { m.header.Name = "workspace/../outside" }),
		"绝对路径":  alter(func(m *member) { m.header.Name = "/outside" }),
		"缺少父目录": alter(func(m *member) { m.header.Name = "workspace/new/child" }),
		"禁止运行锁": alter(func(m *member) { m.header.Name = "data/.tom.lock" }),
		"主密钥别名": alter(func(m *member) { m.header.Name = "data/Master.Key" }),
		"符号链接逃逸": alter(func(m *member) {
			m.header.Typeflag = tar.TypeSymlink
			m.header.Linkname = "../../outside"
			m.header.Size = 0
			m.body = nil
		}),
		"硬链接": alter(func(m *member) {
			m.header.Typeflag = tar.TypeLink
			m.header.Linkname = "data/master.key"
			m.header.Size = 0
			m.body = nil
		}),
		"管道":   alter(func(m *member) { m.header.Typeflag = tar.TypeFifo; m.header.Size = 0; m.body = nil }),
		"权限":   alter(func(m *member) { m.header.Mode = 04755 }),
		"权限被改": alter(func(m *member) { m.header.Mode = 0400 }),
		"内容被改": alter(func(m *member) { m.body = bytes.Repeat([]byte{'x'}, len(m.body)) }),
		"重复成员": func(m []member) []member {
			return append(append(append([]member{}, m[:index]...), m[index]), m[index:]...)
		},
		"截断归档": func(m []member) []member { return m[:len(m)-1] },
		"损坏说明": func(m []member) []member { m[0].body = bytes.Repeat([]byte{'x'}, len(m[0].body)); return m },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			encoded := encodeArchive(t, mutate(append([]member{}, original...)))
			refuseArchive(t, encoded)
		})
	}
	raw, err := os.ReadFile(f.archive)
	must(t, err)
	for name, contents := range map[string][]byte{"压缩尾截断": raw[:len(raw)-1], "尾随内容": append(bytes.Clone(raw), 1), "追加压缩成员": append(bytes.Clone(raw), raw...)} {
		t.Run(name, func(t *testing.T) { refuseArchive(t, contents) })
	}
}

func refuseArchive(t *testing.T, encoded []byte) {
	t.Helper()
	parent := t.TempDir()
	input := filepath.Join(parent, "input")
	must(t, os.WriteFile(input, encoded, 0600))
	sentinel := filepath.Join(parent, "outside")
	must(t, os.WriteFile(sentinel, []byte("边界"), 0600))
	if err := Restore(t.Context(), input, filepath.Join(parent, "new"), ""); err == nil {
		t.Fatal("异常归档被接受")
	}
	children, err := os.ReadDir(parent)
	must(t, err)
	check, err := os.ReadFile(sentinel)
	must(t, err)
	if len(children) != 2 || string(check) != "边界" {
		t.Fatal("未清理暂存目录或改动了外部文件")
	}
}

func TestBackupExcludesRunningServiceAndUnsafeInputs(t *testing.T) {
	f := newInstance(t)
	lock, err := store.LockDirectory(f.data)
	must(t, err)
	if err = Create(t.Context(), f.data, f.workspace, f.archive, ""); err == nil {
		t.Fatal("运行中的服务允许备份")
	}
	must(t, lock.Close())
	if err = Create(t.Context(), f.data, f.workspace, filepath.Join(f.workspace, "自身"), ""); err == nil {
		t.Fatal("允许归档包含自身")
	}
	if err = Create(t.Context(), f.data, filepath.Join(f.data, "."), f.archive, ""); err == nil {
		t.Fatal("接受重叠目录")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = Create(ctx, f.data, f.workspace, f.archive, ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(f.workspace), "outside")
	must(t, os.WriteFile(outside, []byte("边界"), 0600))
	link := filepath.Join(f.workspace, "外部")
	must(t, os.Symlink("../outside", link))
	if err = Create(t.Context(), f.data, f.workspace, f.archive, ""); err == nil {
		t.Fatal("允许外部链接")
	}
	must(t, os.Remove(link))
	must(t, syscall.Mkfifo(link, 0600))
	if err = Create(t.Context(), f.data, f.workspace, f.archive, ""); err == nil {
		t.Fatal("允许特殊文件")
	}
	must(t, os.Remove(link))
	_, err = f.db.Exec("UPDATE schema_migrations SET checksum='' WHERE version=1")
	must(t, err)
	if err = Create(t.Context(), f.data, f.workspace, f.archive, ""); err == nil {
		t.Fatal("允许备份迁移账本已损坏的数据库")
	}
	if _, err = os.Stat(f.archive); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("产生了不完整归档")
	}
}

func TestProofWithoutEncryptedRowsAndConcurrentPublication(t *testing.T) {
	f := newInstance(t)
	env := freshEnvironment(t)
	must(t, Create(t.Context(), f.data, f.workspace, f.archive, env))
	if err := Restore(t.Context(), f.archive, f.target, freshEnvironment(t)); err == nil {
		t.Fatal("空库缺少主密钥绑定")
	}
	before, err := os.ReadFile(f.archive)
	must(t, err)
	if err = Create(t.Context(), f.data, f.workspace, f.archive, env); err == nil {
		t.Fatal("覆盖已有归档")
	}
	after, err := os.ReadFile(f.archive)
	must(t, err)
	if !bytes.Equal(before, after) {
		t.Fatal("已有归档改变")
	}
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- Restore(t.Context(), f.archive, f.target, env) }()
	}
	if (<-results == nil) == (<-results == nil) {
		t.Fatal("并发还原必须仅有一个成功")
	}
}

func TestArchiveLimits(t *testing.T) {
	for _, inventory := range []inventory{{digest: sha256.New(), entries: maxEntries}, {digest: sha256.New(), bytes: maxArchiveBytes}} {
		if inventory.include(&tar.Header{Size: 1}) == nil {
			t.Fatal("容量边界失效")
		}
	}
	if checkedName(strings.Repeat("x/", maxDepth)+"x") == nil {
		t.Fatal("目录深度边界失效")
	}
}

// TestKilledWriterSnapshot 让子进程提交后停在打开的事务里，强制终止以验证 WAL 恢复边界。
func TestKilledWriterSnapshot(t *testing.T) {
	if dir := os.Getenv("TOM_TEST_CRASH_DIRECTORY"); dir != "" {
		db, err := store.Open(t.Context(), filepath.Join(dir, "tom.db"))
		must(t, err)
		_, err = db.Exec("PRAGMA wal_autocheckpoint=0; CREATE TABLE crash_probe (id INTEGER PRIMARY KEY); INSERT INTO crash_probe VALUES(1); BEGIN IMMEDIATE; INSERT INTO crash_probe VALUES(2)")
		must(t, err)
		must(t, os.WriteFile(filepath.Join(dir, "ready"), nil, 0600))
		time.Sleep(time.Minute)
		t.Fatal("未收到终止信号")
	}
	f := newInstance(t)
	must(t, f.db.Close())
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestKilledWriterSnapshot$")
	cmd.Env = append(os.Environ(), "TOM_TEST_CRASH_DIRECTORY="+f.data)
	must(t, cmd.Start())
	defer func() {
		if cmd.ProcessState == nil {
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Error(err)
			}
			if err := cmd.Wait(); err == nil {
				t.Error("子进程意外成功")
			}
		}
	}()
	for {
		if _, err := os.Stat(filepath.Join(f.data, "ready")); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	must(t, cmd.Process.Kill())
	if err := cmd.Wait(); err == nil {
		t.Fatal("写进程未被终止")
	}
	stat, err := os.Stat(filepath.Join(f.data, "tom.db-wal"))
	must(t, err)
	if stat.Size() < 32 {
		t.Fatal("未保留已提交的 WAL 页")
	}
	must(t, Create(t.Context(), f.data, f.workspace, f.archive, ""))
	must(t, Restore(t.Context(), f.archive, f.target, ""))
	db, err := store.Open(t.Context(), filepath.Join(f.target, "data", "tom.db"))
	must(t, err)
	defer func() { must(t, db.Close()) }()
	var rows, sum int
	must(t, db.QueryRow("SELECT count(*),sum(id) FROM crash_probe").Scan(&rows, &sum))
	if rows != 1 || sum != 1 {
		t.Fatal("崩溃恢复提交边界错误")
	}
}

func TestExcludeKeyAliasesAndDanglingOperationalLinks(t *testing.T) {
	f := newInstance(t)
	alias := filepath.Join(f.workspace, "别名")
	must(t, os.Link(filepath.Join(f.data, "master.key"), alias))
	if err := Create(t.Context(), f.data, f.workspace, f.archive, freshEnvironment(t)); err == nil {
		t.Fatal("环境模式归档收录了密钥硬链接")
	}
	must(t, os.Remove(alias))
	must(t, os.Symlink(".tom.lock", filepath.Join(f.data, "运行锁")))
	if err := Create(t.Context(), f.data, f.workspace, f.archive, ""); err == nil {
		t.Fatal("收录了指向排除文件的链接")
	}
}

func TestBackupRejectsWrongKeyMissingAuthAndDeviceCorruption(t *testing.T) {
	f := newInstance(t)
	a, err := auth.New(f.db, f.data)
	must(t, err)
	code, err := a.IssueSetupCode()
	must(t, err)
	password := testkit.Random(t)
	must(t, a.Setup(password, code))
	session, err := a.Login(t.Context(), password, "校验设备", "linux", nil)
	must(t, err)
	must(t, a.UpdatePushToken(t.Context(), session.DeviceID, testkit.Random(t), f.key))
	if err = Create(t.Context(), f.data, f.workspace, f.archive, freshEnvironment(t)); err == nil {
		t.Fatal("真实设备密文与主密钥不匹配时仍产生备份")
	}
	_, err = f.db.Exec("UPDATE devices SET push_token='{}'")
	must(t, err)
	if err = Create(t.Context(), f.data, f.workspace, f.archive, ""); err == nil {
		t.Fatal("无效设备密文被接受")
	}
	_, err = f.db.Exec("UPDATE devices SET push_token=NULL")
	must(t, err)
	must(t, os.Remove(filepath.Join(f.data, "auth.json")))
	if err = Create(t.Context(), f.data, f.workspace, f.archive, ""); err == nil {
		t.Fatal("已有设备但缺少认证文件时仍产生备份")
	}
}

func TestRestoreLinkChainCannotBecomeWriteParent(t *testing.T) {
	f := newInstance(t)
	must(t, Create(t.Context(), f.data, f.workspace, f.archive, ""))
	baseline := readArchive(t, f.archive)
	end := len(baseline) - 1
	link := member{header: *newHeader("workspace/alias", tar.TypeSymlink, 0777, 0, time.Now())}
	link.header.Linkname = "."
	child := member{header: *newHeader("workspace/alias/child", tar.TypeReg, 0600, 0, time.Now())}
	prefix := append(append([]member{}, baseline[:end]...), link, child)
	refuseArchive(t, encodeArchive(t, append(prefix, baseline[end])))
}

func TestRemovePrivateReadonlyStage(t *testing.T) {
	stage := filepath.Join(t.TempDir(), "stage")
	sub := filepath.Join(stage, "readonly")
	must(t, os.MkdirAll(sub, 0700))
	must(t, os.WriteFile(filepath.Join(sub, "内容"), nil, 0400))
	must(t, os.Chmod(sub, 0500))
	must(t, os.Chmod(stage, 0500))
	must(t, removeStage(stage))
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("只读暂存目录未清理")
	}
}

// 验证跨文件提交中断时只读备份和还原都能识别认证暂存状态，首次启动才完成发布。
func TestRestoreSetupCommittedBeforeFilePublication(t *testing.T) {
	f := newInstance(t)
	a, err := auth.New(f.db, f.data)
	must(t, err)
	code, err := a.IssueSetupCode()
	must(t, err)
	password := testkit.Random(t)
	must(t, a.Setup(password, code))
	final := filepath.Join(f.data, "auth.json")
	must(t, os.Rename(final, filepath.Join(f.data, ".auth-setup.pending")))
	must(t, Create(t.Context(), f.data, f.workspace, f.archive, ""))
	if _, err = os.Stat(final); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("快照验证改变源认证文件")
	}
	must(t, Restore(t.Context(), f.archive, f.target, ""))
	directory := filepath.Join(f.target, "data")
	db, err := store.Open(t.Context(), filepath.Join(directory, "tom.db"))
	must(t, err)
	defer func() { must(t, db.Close()) }()
	restored, err := auth.New(db, directory)
	must(t, err)
	if !restored.Initialized() {
		t.Fatal("还原实例未完成认证文件发布")
	}
	_, err = restored.Login(t.Context(), password, "验收", "linux", nil)
	must(t, err)
	var count int
	must(t, db.QueryRow("SELECT count(*) FROM audit_log WHERE action='auth.setup'").Scan(&count))
	if count != 1 {
		t.Fatal("恢复时重复登记初始化审计")
	}
}
