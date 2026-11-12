package dockernode

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computeragent"
)

type probeRecord struct {
	Version       int                      `json:"version"`
	Specification computer.Specification   `json:"specification"`
	Grant         computeragent.ProbeGrant `json:"grant"`
	Phase         string                   `json:"phase"`
}

func (state *probeRecord) validate(userID string) error {
	if state == nil || state.Version != 1 || state.Specification.Validate() != nil || state.Specification.UserID != userID || state.Grant.Validate() != nil {
		return failure("probe_record_invalid")
	}
	specification := state.Specification
	expected := computeragent.Identity{UserID: userID, ComputerID: specification.ComputerID, NodeID: specification.NodeID, Generation: specification.Generation}
	if state.Grant.Identity != expected {
		return failure("probe_record_identity_mismatch")
	}
	switch state.Phase {
	case "prepared", "dispatched", "closing", "closed":
		return nil
	default:
		return failure("probe_record_phase_invalid")
	}
}

func probeRecordName(userID string) (string, error) {
	id, err := ulid.ParseStrict(userID)
	if err != nil || id.String() != userID {
		return "", failure("probe_user_invalid")
	}
	return userID + ".probe.json", nil
}

type probeStore interface {
	readProbe(string) (*probeRecord, error)
	saveProbe(*probeRecord) error
	visitProbes(context.Context, func(string) error) error
}

func (journal *journal) readProbe(userID string) (*probeRecord, error) {
	name, err := probeRecordName(userID)
	if err != nil {
		return nil, err
	}
	encoded, err := journal.readFile(name)
	if err != nil || encoded == nil {
		return nil, err
	}
	var state probeRecord
	if err := agentchannel.Decode(encoded, &state); err != nil {
		return nil, err
	}
	if err := state.validate(userID); err != nil {
		return nil, err
	}
	return &state, nil
}

// saveProbe 在用户生命周期独占锁内调用，保存最后消费版本；不按容器重建重新编号。
func (journal *journal) saveProbe(state *probeRecord) error {
	if state == nil {
		return failure("probe_record_missing")
	}
	name, err := probeRecordName(state.Specification.UserID)
	if err != nil {
		return err
	}
	if err := state.validate(state.Specification.UserID); err != nil {
		return err
	}
	// 保存前复验已有入口，不能用原子替换掩盖文件权限、链接或内容漂移。
	previous, err := journal.readProbe(state.Specification.UserID)
	if err != nil {
		return err
	}
	if previous != nil {
		if state.Grant == previous.Grant {
			allowed := state.Phase == previous.Phase || previous.Phase == "prepared" && (state.Phase == "dispatched" || state.Phase == "closing") || previous.Phase == "dispatched" && state.Phase == "closing" || previous.Phase == "closing" && state.Phase == "closed"
			// JSON 不保存进程内单调时钟；租约按实际到期时间比较，其余身份仍逐项核验。
			if !allowed || !immutableEqual(state.Specification, previous.Specification) || state.Specification.FencingToken != previous.Specification.FencingToken || !state.Specification.LeaseExpiresAt.Equal(previous.Specification.LeaseExpiresAt) {
				return failure("probe_record_transition_invalid")
			}
		} else {
			before, after := previous.Specification, state.Specification
			if previous.Phase != "closed" || state.Phase != "prepared" || state.Grant.Revision <= previous.Grant.Revision || state.Grant.Request.Group.ID == previous.Grant.Request.Group.ID || before.ComputerID != after.ComputerID || before.NodeID != after.NodeID || before.OperationID != after.OperationID || before.VolumeReference != after.VolumeReference || before.Generation > after.Generation || before.Generation == after.Generation && !immutableEqual(before, after) {
				return failure("probe_record_revision_invalid")
			}
		}
	} else if state.Phase != "prepared" {
		return failure("probe_record_initial_phase_invalid")
	}
	encoded, err := agentchannel.Encode(state)
	if err != nil {
		return err
	}
	return journal.writeFile(name, encoded)
}

func (journal *journal) visitProbes(ctx context.Context, visit func(string) error) (failure error) {
	directory, err := journal.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { failure = errors.Join(failure, directory.Close()) }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(100)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".probe.json") {
				continue
			}
			userID := strings.TrimSuffix(entry.Name(), ".probe.json")
			if _, err := probeRecordName(userID); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(userID); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}
