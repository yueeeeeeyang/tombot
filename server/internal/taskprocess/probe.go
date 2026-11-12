package taskprocess

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/processgroup"
)

const ProbeProgram = "/usr/lib/tombot/computer-probe"
const ProbeMaximumLifetime = time.Minute

// ProbeRequest 只授权固定能力探针，不接受命令、环境、UID、目录或 Bot 身份覆盖。
type ProbeRequest struct {
	Version  int                           `json:"version"`
	Group    processgroup.ServiceReference `json:"group"`
	Deadline agentchannel.Deadline         `json:"deadline"`
}

func (request ProbeRequest) Validate() error {
	if request.Version != 1 || request.Group.Validate() != nil || request.Group.Kind != processgroup.ProbeService || request.Deadline.ExpiresNS <= 0 || request.Deadline.TimeNamespace == 0 {
		return errors.New("固定能力探针授权无效")
	}
	return nil
}

func encodeProbe(request ProbeRequest) ([]byte, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > 16<<10 {
		return nil, errors.New("固定能力探针授权超过限制")
	}
	return encoded, nil
}

// DecodeProbe 供固定探针执行进程复验原位执行传递的规范授权，不包含认证材料。
func DecodeProbe(encoded []byte) (ProbeRequest, error) {
	var request ProbeRequest
	if len(encoded) == 0 || len(encoded) > 16<<10 || json.Unmarshal(encoded, &request) != nil {
		return request, errors.New("固定能力探针授权格式无效")
	}
	canonical, err := encodeProbe(request)
	if err != nil || !bytes.Equal(encoded, canonical) {
		return ProbeRequest{}, errors.New("固定能力探针授权不是规范编码")
	}
	return request, nil
}
