package deployment

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/nodeapi"
	"io.yueyang/tom/internal/nodeapp"
	"io.yueyang/tom/internal/noderelease"
)

const updateCache = "/var/lib/tombot-node/releases"

type updateClient struct {
	client                 *http.Client
	origin, node, platform string
	pair                   tls.Certificate
}

func (c updateClient) call(ctx context.Context, action, job, state string, index int, max int64) ([]byte, error) {
	message, err := (noderelease.Call{PackageVersion: 2, Node: c.node, Platform: c.platform, Action: action, Job: job, Index: index, State: state}).Sign(c.pair)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.origin+noderelease.AgentPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.read(req, max)
}
func (c updateClient) read(req *http.Request, max int64) (b []byte, failure error) {
	r, err := c.client.Do(req)
	if err != nil {
		return nil, errors.New("无法连接已绑定的升级服务")
	}
	defer func() { failure = errors.Join(failure, r.Body.Close()) }()
	if r.StatusCode != 200 && r.StatusCode != 204 {
		return nil, fmt.Errorf("节点发布服务拒绝请求（HTTP %d）", r.StatusCode)
	}
	b, err = io.ReadAll(io.LimitReader(r.Body, max+1))
	if int64(len(b)) > max {
		return nil, errors.New("节点发布响应过大")
	}
	return b, err
}

// SyncNodeUpgrade 只执行已绑定服务端核验、同架构且分配给本节点的完整包，不处理命令或外部 URL。
func SyncNodeUpgrade(parent context.Context, logger *slog.Logger) (failure error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errors.New("远程升级需在 Linux 节点以 root 运行")
	}
	ctx, cancel := context.WithTimeout(parent, 235*time.Minute)
	defer cancel()
	fs := filesystem{base: "/", owner: 0}
	unlock, err := fs.lockInstallation("/run/tombot-node-update.lock")
	if err != nil {
		return err
	}
	defer func() { failure = errors.Join(failure, unlock()) }()
	raw, err := fs.readConfig(fileRule{path: nodeInstallationPaths().Node, maximum: 64 << 10, private: true}, account{})
	if err != nil {
		return err
	}
	conf, err := nodeapp.ParseConfig(raw)
	if err != nil || conf.Enrollment == nil {
		return errors.New("节点缺少有效登记配置")
	}
	pair, err := fs.nodeKeyPair()
	if err != nil {
		return err
	}
	pins, err := nodeapi.ParseFingerprints(conf.Enrollment.Server.Fingerprints)
	if err != nil {
		return err
	}
	tlsConfig, err := nodeapi.ClientTLS(pair, pins)
	if err != nil {
		return err
	}
	tr := &http.Transport{TLSClientConfig: tlsConfig}
	defer tr.CloseIdleConnections()
	c := updateClient{&http.Client{Transport: tr, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, conf.Enrollment.Server.URL, conf.NodeID, "linux/" + runtime.GOARCH, pair}
	raw, err = c.call(ctx, "poll", "", "", 0, noderelease.MaxManifest)
	if err != nil || len(raw) == 0 {
		return err
	}
	var a noderelease.Assignment
	if json.Unmarshal(raw, &a) != nil || (computer.NodeQuery{NodeID: a.Job}).Validate() != nil || !noderelease.ValidDigest(a.Release) || a.Manifest.Validate(a.Artifact) != nil {
		return errors.New("升级任务清单无效")
	}
	if err = fs.ensureInstallDirectory(updateCache, 0700); err != nil {
		return err
	}
	previous, err := fs.previousReceipt(a)
	if err != nil {
		return err
	}
	if previous != "" {
		_, err = c.call(ctx, "report", a.Job, previous, 0, 0)
		return err
	}
	state := "failed"
	err = func() error {
		m := a.Manifest
		if m.Platform != c.platform {
			return errors.New("节点与发布包架构不匹配")
		}
		if m.Kind == noderelease.Image {
			installer := nodeInstaller{fs: fs, systemctl: runNodeSystemctl}
			installer.inspectRecovery = installer.inspectNodeRecovery
			installed, err := installer.installedReleaseImage(ctx, m)
			if err != nil {
				return err
			}
			if installed {
				state = "started"
				return nil
			}
		}
		dir := filepath.Join(updateCache, a.Artifact)
		if _, err = os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			if err = c.stage(ctx, dir, a, m); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err = fs.directory(dir, account{}, false, false, false); err != nil {
			return err
		}
		sums, err := fs.readInstallationSource(filepath.Join(dir, noderelease.SumFile(m.Kind)), 64<<10, fs.owner)
		if err != nil || noderelease.Digest(sums) != m.Checksums {
			return errors.New("缓存校验清单与本次升级任务不符")
		}
		path := nodeBinaryPath
		args := []string{"upgrade", "--image-package", dir}
		if m.Kind != noderelease.Image {
			path = filepath.Join(dir, "tomnode")
			program, err := fs.readInstallationSource(path, maximumNodeBinaryBytes, fs.owner)
			if err != nil || !strings.Contains("\n"+string(sums), "\n"+contentDigest(program)+"  tomnode\n") || noderelease.ValidateExecutable(program, c.platform) != nil {
				return errors.New("缓存程序身份或架构无效")
			}
			args = []string{"upgrade"}
		}
		command := exec.CommandContext(ctx, path, args...)
		command.Dir = dir
		command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		command.WaitDelay = 5 * time.Second
		if err = command.Run(); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 3 {
				state = "busy"
			}
			return err
		}
		state = "started"
		return nil
	}()
	if err != nil {
		logger.Warn("节点更新任务未完成", "node_id", conf.NodeID, "job_id", a.Job, "state", state, "reason", err)
	} else {
		logger.Info("节点更新任务完成", "node_id", conf.NodeID, "job_id", a.Job, "state", state)
	}
	if saved := fs.recordReceipt(a, state); saved != nil {
		return errors.Join(err, saved)
	}
	if state == "started" {
		if cleanup := os.RemoveAll(filepath.Join(updateCache, a.Artifact)); cleanup != nil {
			logger.Warn("节点升级缓存未清理", "job_id", a.Job, "node_id", conf.NodeID, "reason", cleanup)
		}
	}
	_, ack := c.call(ctx, "report", a.Job, state, 0, 0)
	return errors.Join(err, ack)
}

// stage 流式验证升级任务指定的每个分片，再解压并原子发布；重复领取同一包复用私有暂存结果。
func (c updateClient) stage(ctx context.Context, dest string, a noderelease.Assignment, m noderelease.Manifest) (failure error) {
	stage, err := os.MkdirTemp(filepath.Dir(dest), ".download-")
	if err != nil {
		return err
	}
	defer func() { failure = errors.Join(failure, os.RemoveAll(stage)) }()
	file, err := os.CreateTemp(stage, "archive-")
	if err != nil {
		return err
	}
	defer func() { failure = errors.Join(failure, file.Close()) }()
	for i, d := range m.Chunks {
		b, err := c.call(ctx, "part", a.Job, "", i, m.PartSize(i))
		if err != nil {
			return err
		}
		if int64(len(b)) != m.PartSize(i) || noderelease.Digest(b) != d {
			return errors.New("升级包分片摘要不符")
		}
		if _, err = file.Write(b); err != nil {
			return err
		}
	}
	if _, err = file.Seek(0, 0); err != nil {
		return err
	}
	payload := filepath.Join(stage, "package")
	if err = os.Mkdir(payload, 0700); err != nil {
		return err
	}
	if err = noderelease.Extract(ctx, file, payload); err != nil {
		return err
	}
	if err = syncDirectory(payload); err != nil {
		return err
	}
	if err = publishPath(payload, dest); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(dest))
}

// installedReleaseImage 仅复用已提交、已复核且无待恢复安装事务的相同镜像；中断事务仍进入原升级流程。
func (installer nodeInstaller) installedReleaseImage(ctx context.Context, manifest noderelease.Manifest) (installed bool, failure error) {
	fs := installer.fs
	unlock, err := fs.lockNodeInstallation()
	if err != nil {
		return false, err
	}
	defer func() { failure = errors.Join(failure, unlock()) }()
	if _, err = os.Lstat(fs.resolve(nodeRepairDirectory)); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	record, err := installer.installedNodeRecord()
	if err != nil {
		return false, err
	}
	req, digest, err := parseNodeInstallRequest(record.Request)
	if err != nil || digest != record.RequestSHA256 {
		return false, errors.New("镜像复用前无法核对节点安装记录")
	}
	if !req.Programs.valid() || req.Image.Reference != manifest.ImageReference || req.Image.ArchiveSHA256 != manifest.ArchiveSHA256 {
		return false, nil
	}
	runtimeDigest, err := noderelease.RuntimeDigest(req.Docker)
	if err != nil || runtimeDigest != manifest.RuntimeSHA256 {
		return false, err
	}
	if _, err := installer.verifyInstallation(ctx, record); err != nil {
		return false, err
	}
	status, err := installer.inspectRecovery(ctx, req, manifest.Checksums)
	if err != nil {
		return false, err
	}
	if status == nil || status.State != "clear" {
		return false, nil
	}
	if err := installer.systemctl(ctx, "is-active", "--quiet", nodeServiceName); err != nil {
		// 未启动时交回完整安装流程，由其启动节点并验收。
		return false, nil
	}
	return true, nil
}
