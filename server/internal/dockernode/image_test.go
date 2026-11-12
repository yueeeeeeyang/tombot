package dockernode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

func testImage(reference, platform string) imageDescription {
	image := imageDescription{ID: "sha256:" + strings.Repeat("b", 64), RepoDigests: []string{reference}, OS: "linux", Architecture: strings.TrimPrefix(platform, "linux/")}
	image.Config.Entrypoint = []string{"/usr/lib/tombot/computer-agent"}
	image.Config.WorkingDir = "/workspace"
	image.Config.Labels = map[string]string{computer.StorageLayoutLabel: computer.StorageLayoutVersion}
	return image
}

func imageManifestFixture() ImageManifest {
	return ImageManifest{Version: 1, NodeID: ids.New(), ImageID: ids.New(), Reference: "registry.example.test/computer@sha256:" + strings.Repeat("a", 64), Platform: "linux/" + runtime.GOARCH, Socket: "/run/docker.sock", StateDirectory: "/var/lib/tombot-node/images", CallTimeoutSeconds: 10, TimeoutSeconds: 600}
}

func TestImageManifestValidation(t *testing.T) {
	manifest := imageManifestFixture()
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ParseImageManifest(encoded)
	if err != nil || decoded != manifest {
		t.Fatal(decoded, err)
	}
	for _, mutate := range []func(*ImageManifest){
		func(m *ImageManifest) { m.Version = 0 }, func(m *ImageManifest) { m.NodeID = "../bad" }, func(m *ImageManifest) { m.ImageID = strings.ToLower(m.ImageID) }, func(m *ImageManifest) { m.Reference = "registry.example.test/computer:latest" }, func(m *ImageManifest) { m.Platform = "linux/386" }, func(m *ImageManifest) { m.Socket = "relative" }, func(m *ImageManifest) { m.StateDirectory = "/var/../tmp" }, func(m *ImageManifest) { m.AuthenticationFile = "not-absolute" }, func(m *ImageManifest) { m.CallTimeoutSeconds = 0 }, func(m *ImageManifest) { m.TimeoutSeconds = 3601 }, func(m *ImageManifest) { m.CallTimeoutSeconds = 300; m.TimeoutSeconds = 30 },
	} {
		changed := manifest
		mutate(&changed)
		if changed.validate() == nil {
			t.Fatal("接受无效镜像配置")
		}
	}
	for _, invalid := range []string{"{}", `{"version":1,"version":1}`, `{"Version":1}`, `{"version":null}`, `{"unknown":1}`, string(encoded) + "{}", `[]`} {
		if _, err := ParseImageManifest([]byte(invalid)); err == nil {
			t.Fatal("接受歧义镜像配置")
		}
	}
}

func TestImageMetadataBoundaries(t *testing.T) {
	manifest := imageManifestFixture()
	image := testImage(manifest.Reference, manifest.Platform)
	if err := image.validate(manifest.Reference, manifest.Platform); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*imageDescription){
		func(i *imageDescription) { i.Config.Labels = nil }, func(i *imageDescription) { i.Config.Labels = map[string]string{computer.StorageLayoutLabel: "1"} },
		func(i *imageDescription) { i.ID = "sha256:" + strings.Repeat("x", 64) }, func(i *imageDescription) { i.RepoDigests = nil }, func(i *imageDescription) { i.RepoDigests = []string{"other@sha256:" + strings.Repeat("a", 64)} }, func(i *imageDescription) { i.OS = "windows" }, func(i *imageDescription) { i.Architecture = "386" }, func(i *imageDescription) { i.Variant = "v7" }, func(i *imageDescription) { i.Config.Entrypoint = []string{"/bin/sh"} }, func(i *imageDescription) { i.Config.Cmd = []string{"unexpected"} }, func(i *imageDescription) { i.Config.WorkingDir = "/" }, func(i *imageDescription) { i.Config.User = "1000" },
	} {
		changed := image
		mutate(&changed)
		if changed.validate(manifest.Reference, manifest.Platform) == nil {
			t.Fatal("无效镜像获得准备通过")
		}
	}
	image = testImage(manifest.Reference, "linux/arm64")
	image.Variant = "v8"
	if err := image.validate(manifest.Reference, "linux/arm64"); err != nil {
		t.Fatal(err)
	}
}

type fakeImageEngine struct {
	image                   imageDescription
	exists                  bool
	inspectError, pullError error
	verificationError       error
	pulls, inspections      int
	pull                    func(context.Context) error
	authentication          string
}

func (engine *fakeImageEngine) inspectImage(ctx context.Context, _ string) (imageDescription, error) {
	engine.inspections++
	if engine.inspections > 1 && engine.verificationError != nil {
		return imageDescription{}, engine.verificationError
	}
	if ctx.Err() != nil {
		return imageDescription{}, ctx.Err()
	}
	if engine.inspectError != nil {
		return imageDescription{}, engine.inspectError
	}
	if !engine.exists {
		return imageDescription{}, errMissing
	}
	return engine.image, nil
}
func (engine *fakeImageEngine) pullImage(ctx context.Context, _, _, authentication string) error {
	engine.pulls++
	engine.authentication = authentication
	if engine.pull != nil {
		if err := engine.pull(ctx); err != nil {
			return err
		}
	}
	if engine.pullError != nil {
		return engine.pullError
	}
	engine.exists = true
	return ctx.Err()
}

func imagePreparerFixture(t *testing.T) (imagePreparer, ImageManifest, *fakeImageEngine, *journal) {
	t.Helper()
	manifest := imageManifestFixture()
	journal, err := openJournal(t.TempDir(), uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.root.Close(); err != nil {
			t.Error(err)
		}
	})
	engine := &fakeImageEngine{image: testImage(manifest.Reference, manifest.Platform)}
	preparer := imagePreparer{engine: engine, store: imageJournal{journal}, readAuthentication: func(string) ([]byte, error) { t.Fatal("公开镜像读取了认证文件"); return nil, nil }}
	return preparer, manifest, engine, journal
}

func TestImagePreparationReconcilesActualEngine(t *testing.T) {
	preparer, manifest, engine, _ := imagePreparerFixture(t)
	engine.pull = func(context.Context) error {
		state, err := preparer.store.load(manifest)
		if err != nil || state == nil || state.State != "preparing" {
			t.Fatal("未保存意图就开始拉取", err)
		}
		return nil
	}
	for range 2 {
		report, err := preparer.prepare(t.Context(), manifest)
		if err != nil || report.State != "available" || report.Scope != "image" || report.DockerImageID != engine.image.ID {
			t.Fatal(report, err)
		}
	}
	if engine.pulls != 1 || engine.inspections != 3 {
		t.Fatal("未复核实际状态或重复拉取", engine.pulls, engine.inspections)
	}
	engine.exists = false
	if _, err := preparer.prepare(t.Context(), manifest); err != nil || engine.pulls != 2 {
		t.Fatal("旧成功记录掩盖镜像被删除", err)
	}
}

type failedImageStore struct {
	imagePreparationStore
	state string
}

func (store failedImageStore) save(manifest ImageManifest, state ImagePreparation) error {
	if state.State == store.state {
		return errors.New("测试镜像记录写入失败")
	}
	return store.imagePreparationStore.save(manifest, state)
}

func TestImagePreparationFailuresRetainIdentity(t *testing.T) {
	for _, stage := range []string{"preparing", "inspect", "pull", "verify", "metadata", "available", "cancelled", "auth"} {
		t.Run(stage, func(t *testing.T) {
			preparer, manifest, engine, _ := imagePreparerFixture(t)
			base := preparer.store
			preparer.store = failedImageStore{base, stage}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			injected := errors.New("测试镜像失败")
			switch stage {
			case "inspect":
				engine.inspectError = injected
			case "pull":
				engine.pullError = injected
			case "verify":
				engine.verificationError = injected
			case "metadata":
				engine.image.RepoDigests = nil
			case "cancelled":
				engine.pull = func(context.Context) error { cancel(); return ctx.Err() }
			case "auth":
				manifest.AuthenticationFile = "/private/auth.json"
				preparer.readAuthentication = func(string) ([]byte, error) { return nil, injected }
			}
			report, err := preparer.prepare(ctx, manifest)
			if err == nil || report != (ImagePreparation{}) {
				t.Fatal("失败返回可用回执", report, err)
			}
			state, err := base.load(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "preparing" {
				if state != nil || engine.inspections != 0 || engine.pulls != 0 {
					t.Fatal("持久意图失败仍访问引擎")
				}
				return
			}
			if state == nil || state.State != "failed" || state.ImageID != manifest.ImageID {
				t.Fatal("未知结果丢失或身份改变", state)
			}
			if engine.pulls > 1 {
				t.Fatal("未知请求被自动重发")
			}
		})
	}
}

func TestImagePreparationLostPullResponse(t *testing.T) {
	preparer, manifest, engine, _ := imagePreparerFixture(t)
	engine.pull = func(context.Context) error { engine.exists = true; return context.DeadlineExceeded }
	if _, err := preparer.prepare(t.Context(), manifest); err == nil {
		t.Fatal("丢失响应仍成功")
	}
	if report, err := preparer.prepare(t.Context(), manifest); err != nil || report.State != "available" || engine.pulls != 1 {
		t.Fatal("未先对账便补发拉取", report, err)
	}
}

func TestImageRecordCannotBeRebound(t *testing.T) {
	for _, kind := range []string{"node", "reference", "platform", "digest", "corrupt", "symlink", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			preparer, manifest, engine, journal := imagePreparerFixture(t)
			if _, err := preparer.prepare(t.Context(), manifest); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(journal.root.Name(), manifest.ImageID+".image.json")
			switch kind {
			case "node":
				manifest.NodeID = ids.New()
			case "reference":
				manifest.Reference = strings.Replace(manifest.Reference, "/computer@", "/other@", 1)
			case "platform":
				if manifest.Platform == "linux/arm64" {
					manifest.Platform = "linux/amd64"
				} else {
					manifest.Platform = "linux/arm64"
				}
			case "digest":
				engine.image.ID = "sha256:" + strings.Repeat("c", 64)
			case "corrupt":
				if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".retained", path); err != nil {
					t.Fatal(err)
				}
			}
			if report, err := preparer.prepare(t.Context(), manifest); err == nil || report != (ImagePreparation{}) {
				t.Fatal("镜像准备记录允许重绑", report, err)
			}
			if engine.pulls != 1 {
				t.Fatal("身份冲突触发拉取")
			}
		})
	}
}

func TestImageAuthenticationScopedAndPrivate(t *testing.T) {
	preparer, manifest, engine, _ := imagePreparerFixture(t)
	manifest.AuthenticationFile = "/private/registry.json"
	// 仅生成本次测试的无效认证值，仓库不会接收这些值。
	marker := ids.New()
	encoded, err := json.Marshal(map[string]string{"serveraddress": "registry.example.test", "identitytoken": marker})
	if err != nil {
		t.Fatal(err)
	}
	preparer.readAuthentication = func(path string) ([]byte, error) {
		if path != manifest.AuthenticationFile {
			t.Fatal("认证文件来源被覆盖")
		}
		return bytes.Clone(encoded), nil
	}
	if _, err := preparer.prepare(t.Context(), manifest); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.URLEncoding.DecodeString(engine.authentication)
	if err != nil || !bytes.Contains(decoded, []byte(marker)) {
		t.Fatal("认证头编码不符")
	}
	for _, invalid := range []string{`{}`, `{"serveraddress":"other.test","identitytoken":"` + marker + `"}`, `{"serveraddress":"registry.example.test","identitytoken":"` + marker + `","username":"a"}`, `{"serveraddress":"registry.example.test","serveraddress":"registry.example.test"}`} {
		if value, err := imageAuthentication([]byte(invalid), manifest.Reference); err == nil || value != "" || strings.Contains(err.Error(), marker) {
			t.Fatal("错误认证被接受或泄露")
		}
	}
}

func TestImagePullStreamAndProtocol(t *testing.T) {
	manifest := imageManifestFixture()
	for _, body := range []string{"", `{"error":"private-value"}`, `{"status":"pulling","errorDetail":{"message":"private-value"}}`, "{", `{} {}`, strings.Repeat("x", 64<<10)} {
		if err := readImageProgress(strings.NewReader(body)); err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatal("流失败被接受或泄露", err)
		}
	}
	if err := readImageProgress(strings.NewReader("{\"status\":\"pulling\",\"id\":\"layer\"}\n{\"status\":\"complete\",\"newField\":true}\n")); err != nil {
		t.Fatal(err)
	}
	status := 200
	engine := &dockerEngine{client: &http.Client{Transport: roundTrip(func(request *http.Request) (*http.Response, error) {
		if request.Method != "POST" || request.URL.Path != "/v1.47/images/create" || request.URL.Query().Get("fromImage") != manifest.Reference || request.URL.Query().Get("platform") != manifest.Platform || len(request.URL.Query()) != 2 || request.Body != nil {
			t.Fatal("镜像拉取参数越界")
		}
		if _, ok := request.Context().Deadline(); !ok {
			t.Fatal("拉取没有期限")
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{\"status\":\"complete\"}\n")), Header: make(http.Header)}, nil
	})}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := engine.pullImage(ctx, manifest.Reference, manifest.Platform, ""); err != nil {
		t.Fatal(err)
	}
	status = 500
	if err := engine.pullImage(ctx, manifest.Reference, manifest.Platform, ""); err == nil {
		t.Fatal("引擎错误仍成功")
	}
	if err := engine.pullImage(context.Background(), manifest.Reference, manifest.Platform, ""); err == nil {
		t.Fatal("无期限拉取被接受")
	}
}

func TestEnsureRequiresLocalImageBeforeAllocating(t *testing.T) {
	f := newFixture(t)
	f.engine.imageError = errMissing
	if _, err := f.node.Ensure(t.Context(), f.specification); !errors.Is(err, errMissing) {
		t.Fatal(err)
	}
	if len(f.storage.workspaces) != 0 || len(f.engine.events) != 0 {
		t.Fatal("镜像缺失仍创建用户磁盘或容器")
	}
}

type endlessImageProgress struct {
	line         []byte
	offset, read int
}

func (reader *endlessImageProgress) Read(buffer []byte) (int, error) {
	count := 0
	for count < len(buffer) {
		copied := copy(buffer[count:], reader.line[reader.offset:])
		reader.offset = (reader.offset + copied) % len(reader.line)
		count += copied
	}
	reader.read += count
	return count, nil
}

func TestImagePullRejectsUnboundedProgress(t *testing.T) {
	reader := &endlessImageProgress{line: []byte("{\"status\":\"" + strings.Repeat("x", 32000) + "\"}\n")}
	if err := readImageProgress(reader); err == nil {
		t.Fatal("无限进度流被当作成功")
	}
	if reader.read > (64<<20)+1 {
		t.Fatal("读取进度流超过总量限制", reader.read)
	}
}
