// Package computerprobe 在用户容器的独立非 root 服务进程中核实基础能力，不自行声明整台电脑就绪。
package computerprobe

import (
	"context"
	"errors"

	"github.com/oklog/ulid/v2"

	"io.yueyang/tom/internal/taskprocess"
)

// Evidence 仅记录实际检查证据；磁盘硬容量、进程回收、出网代理及共享浏览器仍由节点组合核验。
type Evidence struct {
	ServiceID      string `json:"service_id"`
	Terminal       bool   `json:"terminal"`
	Files          bool   `json:"files"`
	LocalNetwork   bool   `json:"local_network"`
	FilesystemType int64  `json:"filesystem_type"`
	FilesystemSize uint64 `json:"filesystem_size"`
}

// Validate 拒绝部分成功、其他实例或缺失文件系统数值；它不证明宿主已完成回收。
func (evidence Evidence) Validate(serviceID string) error {
	id, err := ulid.ParseStrict(serviceID)
	if err != nil || id.String() != serviceID || evidence.ServiceID != serviceID || !evidence.Terminal || !evidence.Files || !evidence.LocalNetwork || evidence.FilesystemType == 0 || evidence.FilesystemSize == 0 {
		return errors.New("电脑基础探针证据无效")
	}
	return nil
}

// Run 先复验固定身份、真实服务组与授权期限，再执行探针；任一步失败不返回部分成功证据。
func Run(ctx context.Context, request taskprocess.ProbeRequest) (Evidence, error) {
	if err := ctx.Err(); err != nil {
		return Evidence{}, err
	}
	deadline, err := taskprocess.CheckProbe(request)
	if err != nil {
		return Evidence{}, errors.New("能力探针运行身份、资源组或期限无效")
	}
	bounded, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	evidence, err := inspect(bounded)
	if err != nil {
		return Evidence{}, err
	}
	if _, err = taskprocess.CheckProbe(request); err != nil || bounded.Err() != nil {
		return Evidence{}, errors.New("能力探针完成时授权已结束或归属变化")
	}
	evidence.ServiceID = request.Group.ID
	return evidence, nil
}
