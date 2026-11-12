package taskprocess

import (
	"errors"
	"strconv"
	"strings"
)

// processStatus 只解析隔离所需字段，不保存可能含用户信息的完整进程状态。
type processStatus struct {
	uid, gid                 [4]uint32
	groups                   []uint32
	capabilities             map[string]uint64
	noNewPrivileges, seccomp uint64
}

func parseStatus(encoded []byte) (processStatus, error) {
	status := processStatus{capabilities: make(map[string]uint64)}
	if len(encoded) == 0 || len(encoded) > 64<<10 {
		return status, errors.New("任务进程状态大小无效")
	}
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(encoded), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch name {
		case "Uid", "Gid", "Groups", "CapInh", "CapPrm", "CapEff", "CapAmb", "NoNewPrivs", "Seccomp":
		default:
			continue
		}
		if seen[name] {
			return status, errors.New("任务进程状态字段重复")
		}
		seen[name] = true
		parts := strings.Fields(value)
		if name == "Uid" || name == "Gid" {
			if len(parts) != 4 {
				return status, errors.New("任务进程身份状态无效")
			}
			identities := &status.uid
			if name == "Gid" {
				identities = &status.gid
			}
			for index, part := range parts {
				number, err := strconv.ParseUint(part, 10, 32)
				if err != nil {
					return status, errors.New("任务进程身份数值无效")
				}
				identities[index] = uint32(number)
			}
			continue
		}
		if name == "Groups" {
			if len(parts) > 256 {
				return status, errors.New("任务进程附加组超过限制")
			}
			for _, part := range parts {
				number, err := strconv.ParseUint(part, 10, 32)
				if err != nil {
					return status, errors.New("任务进程附加组无效")
				}
				status.groups = append(status.groups, uint32(number))
			}
			continue
		}
		if len(parts) != 1 {
			return status, errors.New("任务进程隔离状态无效")
		}
		base := 10
		if strings.HasPrefix(name, "Cap") {
			base = 16
		}
		number, err := strconv.ParseUint(parts[0], base, 64)
		if err != nil {
			return status, errors.New("任务进程隔离数值无效")
		}
		switch name {
		case "NoNewPrivs":
			status.noNewPrivileges = number
		case "Seccomp":
			status.seccomp = number
		default:
			status.capabilities[name] = number
		}
	}
	if len(seen) != 9 {
		return status, errors.New("任务进程隔离状态不完整")
	}
	return status, nil
}

func (status processStatus) isolated() bool {
	return status.noNewPrivileges == 1 && status.seccomp == 2
}

func (status processStatus) unprivileged(uid, gid uint32) bool {
	if !status.isolated() || status.uid != [4]uint32{uid, uid, uid, uid} || status.gid != [4]uint32{gid, gid, gid, gid} || len(status.groups) != 0 {
		return false
	}
	for _, capability := range status.capabilities {
		if capability != 0 {
			return false
		}
	}
	return true
}
