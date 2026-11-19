package computer

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/oklog/ulid/v2"
)

// Maintenance 只接受已登记的原电脑身份；提交重启须携带读取状态时得到的版本。
type Maintenance struct {
	UserID          string `json:"user_id"`
	ComputerID      string `json:"computer_id"`
	NodeID          string `json:"node_id"`
	ContainerID     string `json:"container_id"`
	Generation      int64  `json:"generation"`
	ExpectedVersion string `json:"expected_version,omitempty"`
}

func (m Maintenance) Validate() error {
	for _, id := range []string{m.UserID, m.ComputerID, m.NodeID} {
		if _, err := ulid.ParseStrict(id); err != nil {
			return errors.New("电脑归属无效")
		}
	}
	for _, id := range []string{m.ContainerID, m.ExpectedVersion} {
		if id == "" {
			continue
		}
		if _, err := hex.DecodeString(id); err != nil || len(id) != 64 {
			return errors.New("电脑实例或版本无效")
		}
	}
	if m.Generation < 1 || m.ContainerID == "" {
		return errors.New("电脑实例缺失")
	}
	return nil
}

type ComputerStatus struct {
	State   string `json:"state"`
	Version string `json:"version"`
}
type MaintenanceReceipt struct {
	Target Maintenance `json:"target"`
	ComputerStatus
}

func (r MaintenanceReceipt) Matches(m Maintenance) bool {
	if r.Target != m || len(r.Version) != 64 {
		return false
	}
	if _, err := hex.DecodeString(r.Version); err != nil {
		return false
	}
	switch r.State {
	case "running", "stopped", "restarting", "failed":
		return true
	}
	return false
}

type ComputerManager interface {
	ManageComputer(context.Context, Maintenance) (MaintenanceReceipt, error)
}
