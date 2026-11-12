package dockernode

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"io.yueyang/tom/internal/agentchannel"
)

// ImagePreparation 仅证明该节点本地存在指定平台镜像；不提供设置电脑或节点就绪的字段。
type ImagePreparation struct {
	Version       int    `json:"version"`
	Scope         string `json:"scope"`
	NodeID        string `json:"node_id"`
	ImageID       string `json:"image_id"`
	Reference     string `json:"reference"`
	Platform      string `json:"platform"`
	DockerImageID string `json:"docker_image_id,omitempty"`
	State         string `json:"state"`
}

func (state ImagePreparation) validate(manifest ImageManifest) error {
	if state.Version != 1 || state.Scope != "image" || state.NodeID != manifest.NodeID || state.ImageID != manifest.ImageID || state.Reference != manifest.Reference || state.Platform != manifest.Platform {
		return failure("image_preparation_identity_mismatch")
	}
	if state.State != "preparing" && state.State != "available" && state.State != "failed" {
		return failure("image_preparation_state_invalid")
	}
	if state.DockerImageID == "" && state.State == "available" || state.DockerImageID != "" && !validDockerImageID(state.DockerImageID) {
		return failure("image_preparation_digest_invalid")
	}
	return nil
}

type imageEngine interface {
	inspectImage(context.Context, string) (imageDescription, error)
	pullImage(context.Context, string, string, string) error
}

type imagePreparationStore interface {
	load(ImageManifest) (*ImagePreparation, error)
	save(ImageManifest, ImagePreparation) error
}

type imageJournal struct{ journal *journal }

func (store imageJournal) load(manifest ImageManifest) (*ImagePreparation, error) {
	encoded, err := store.journal.readFile(manifest.ImageID + ".image.json")
	if err != nil || encoded == nil {
		return nil, err
	}
	var state ImagePreparation
	if err := agentchannel.Decode(encoded, &state); err != nil {
		return nil, failure("image_preparation_record_invalid")
	}
	if err := state.validate(manifest); err != nil {
		return nil, err
	}
	return &state, nil
}

func (store imageJournal) save(manifest ImageManifest, state ImagePreparation) error {
	if err := state.validate(manifest); err != nil {
		return err
	}
	previous, err := store.load(manifest)
	if err != nil {
		return err
	}
	if previous != nil && previous.DockerImageID != "" && previous.DockerImageID != state.DockerImageID {
		return failure("image_preparation_digest_changed")
	}
	encoded, err := agentchannel.Encode(state)
	if err != nil {
		return failure("image_preparation_encoding_failed")
	}
	return store.journal.writeFile(manifest.ImageID+".image.json", encoded)
}

type imagePreparer struct {
	engine             imageEngine
	store              imagePreparationStore
	readAuthentication func(string) ([]byte, error)
}

// prepare 在持久锁内执行；成功记录也必须复验引擎，失败保留原身份，重启先检查再决定是否拉取。
func (preparer imagePreparer) prepare(ctx context.Context, manifest ImageManifest) (report ImagePreparation, result error) {
	if err := manifest.validate(); err != nil {
		return report, err
	}
	previous, err := preparer.store.load(manifest)
	if err != nil {
		return report, err
	}
	state := ImagePreparation{Version: 1, Scope: "image", NodeID: manifest.NodeID, ImageID: manifest.ImageID, Reference: manifest.Reference, Platform: manifest.Platform, State: "preparing"}
	if previous != nil {
		state.DockerImageID = previous.DockerImageID
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if err := preparer.store.save(manifest, state); err != nil {
		return report, err
	}
	defer func() {
		if result != nil {
			state.State = "failed"
			result = errors.Join(result, preparer.store.save(manifest, state))
			report = ImagePreparation{}
		}
	}()
	image, err := preparer.engine.inspectImage(ctx, manifest.Reference)
	if errors.Is(err, errMissing) {
		authentication, err := preparer.authentication(manifest)
		if err != nil {
			return report, err
		}
		if err := preparer.engine.pullImage(ctx, manifest.Reference, manifest.Platform, authentication); err != nil {
			return report, err
		}
		image, err = preparer.engine.inspectImage(ctx, manifest.Reference)
		if err != nil {
			return report, err
		}
	} else if err != nil {
		return report, err
	}
	if err := image.validate(manifest.Reference, manifest.Platform); err != nil {
		return report, err
	}
	if state.DockerImageID != "" && state.DockerImageID != image.ID {
		return report, failure("image_preparation_digest_changed")
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	state.State, state.DockerImageID = "available", image.ID
	if err := preparer.store.save(manifest, state); err != nil {
		return report, err
	}
	return state, nil
}

func (preparer imagePreparer) authentication(manifest ImageManifest) (string, error) {
	if manifest.AuthenticationFile == "" {
		return "", nil
	}
	encoded, err := preparer.readAuthentication(manifest.AuthenticationFile)
	if err != nil {
		return "", err
	}
	defer clear(encoded)
	return imageAuthentication(encoded, manifest.Reference)
}

// PrepareImage 是 root 本机部署入口；只接受受保护清单，返回镜像可用回执或明确失败，不启动容器。
func PrepareImage(ctx context.Context, path string, logger *slog.Logger) (report ImagePreparation, result error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 || logger == nil {
		return report, failure("image_preparation_requires_linux_root")
	}
	encoded, err := ReadDeploymentFile(path)
	if err != nil {
		return report, err
	}
	manifest, err := ParseImageManifest(encoded)
	if err != nil {
		return report, err
	}
	if manifest.Platform != "linux/"+runtime.GOARCH {
		return report, failure("image_preparation_host_platform_mismatch")
	}
	for _, directory := range []string{manifest.StateDirectory, filepath.Dir(manifest.Socket)} {
		if err := checkProtectedParents(directory); err != nil {
			return report, err
		}
	}
	if err := checkRootDockerSocket(manifest.Socket); err != nil {
		return report, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(manifest.TimeoutSeconds)*time.Second)
	defer cancel()
	journal, err := openJournal(manifest.StateDirectory, 0)
	if err != nil {
		return report, err
	}
	defer func() {
		result = errors.Join(result, journal.root.Close())
		if result != nil {
			report = ImagePreparation{}
		}
	}()
	release, err := journal.lock(ctx, manifest.ImageID)
	if err != nil {
		return report, err
	}
	defer func() { result = errors.Join(result, release()) }()
	engine, err := newEngine(manifest.Socket, time.Duration(manifest.CallTimeoutSeconds)*time.Second)
	if err != nil {
		return report, err
	}
	defer engine.close()
	logger.Info("开始准备电脑镜像", "node_id", manifest.NodeID, "image_id", manifest.ImageID, "platform", manifest.Platform)
	report, err = (imagePreparer{engine: engine, store: imageJournal{journal}, readAuthentication: ReadDeploymentFile}).prepare(ctx, manifest)
	if err != nil {
		logger.Error("电脑镜像准备失败", "node_id", manifest.NodeID, "image_id", manifest.ImageID)
		return ImagePreparation{}, err
	}
	logger.Info("电脑镜像已在本地核实", "node_id", manifest.NodeID, "image_id", manifest.ImageID, "docker_image_id", report.DockerImageID)
	return report, nil
}
