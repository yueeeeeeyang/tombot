//go:build linux

package computeragent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computerprobe"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/taskprocess"
)

// OpenProbe 仅验证固定探针入口，不创建桌面目录；成功后才接管描述符。
func (factory *kernelScopes) OpenProbe(reference processgroup.ServiceReference, files []*os.File) (ProbeScope, error) {
	if reference.Kind != processgroup.ProbeService {
		return nil, errors.New("固定探针不能使用其他基础服务组")
	}
	return factory.openService(reference, files)
}

func (scope *kernelServiceScope) Run(ctx context.Context, request taskprocess.ProbeRequest, revocation *os.File) (computerprobe.Evidence, error) {
	scope.mu.Lock()
	if request.Group != scope.reference || request.Group.Kind != processgroup.ProbeService || scope.used || scope.revoked || scope.closed {
		scope.mu.Unlock()
		return computerprobe.Evidence{}, errors.New("探针资源组已经使用或关闭")
	}
	scope.used = true
	scope.mu.Unlock()
	return runProbeProcess(ctx, request, scope.files[0], revocation)
}

// probeOutput 继续排空超限输出但只保存上限内容，避免子进程因满管道阻塞或内存无界增长。
type probeOutput struct {
	content  []byte
	exceeded bool
}

func (output *probeOutput) Len() int      { return len(output.content) }
func (output *probeOutput) Bytes() []byte { return output.content }

const maximumProbeOutputBytes = 4096

func (output *probeOutput) Write(content []byte) (int, error) {
	remaining := maximumProbeOutputBytes - output.Len()
	if len(content) > remaining {
		output.exceeded = true
	}
	if remaining > 0 {
		output.content = append(output.content, content[:min(remaining, len(content))]...)
	}
	return len(content), nil
}

func runProbeProcess(ctx context.Context, request taskprocess.ProbeRequest, membership, revocation *os.File) (evidence computerprobe.Evidence, failure error) {
	sealed, err := taskprocess.SealProbe(request)
	if err != nil {
		return evidence, err
	}
	defer func() {
		failure = errors.Join(failure, sealed.Close())
		if failure != nil {
			evidence = computerprobe.Evidence{}
		}
	}()
	command := exec.CommandContext(ctx, taskprocess.ProbeProgram)
	command.Dir = "/"
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	command.ExtraFiles = []*os.File{sealed, membership, revocation}
	// 正式程序不启动外部命令；仍限制异常后代占用标准流时的 Wait 等待。
	command.WaitDelay = time.Second
	var output, diagnostic probeOutput
	command.Stdout, command.Stderr = &output, &diagnostic
	if err = command.Run(); err != nil {
		return evidence, errors.New("固定能力探针执行失败或取消")
	}
	if output.exceeded || diagnostic.exceeded || diagnostic.Len() != 0 {
		return evidence, errors.New("固定能力探针输出超过限制或包含异常")
	}
	encoded := output.Bytes()
	if len(encoded) < 2 || encoded[len(encoded)-1] != '\n' {
		return evidence, errors.New("固定能力探针输出不完整")
	}
	if err = agentchannel.Decode(encoded[:len(encoded)-1], &evidence); err != nil {
		return computerprobe.Evidence{}, err
	}
	if err = evidence.Validate(request.Group.ID); err != nil {
		return computerprobe.Evidence{}, err
	}
	return evidence, ctx.Err()
}

var _ ProbeFactory = (*kernelScopes)(nil)
