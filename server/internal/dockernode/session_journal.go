package dockernode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

// sessionKey 将关闭屏障和租约绑定到用户下的稳定桌面，不能按本次运行或容器代次重新编号。
type sessionKey struct {
	userID, desktopID string
}

func sessionKeyOf(session computer.ExecutionSession) sessionKey {
	return sessionKey{session.UserID, session.DesktopID}
}

func (key sessionKey) validate() error {
	for _, id := range []string{key.userID, key.desktopID} {
		if _, err := ulid.ParseStrict(id); err != nil {
			return errors.New("电脑桌面记录标识无效")
		}
	}
	return nil
}

func (key sessionKey) name() string { return key.userID + "." + key.desktopID + ".session" }

// lockSession 按生命周期共享锁、桌面独占锁的固定顺序加锁，跨桌面调用可以并行。
// 任何一步失败都释放已经取得的锁；删除容器必须等待所有桌面操作结束。
func (journal *journal) lockSession(ctx context.Context, key sessionKey) (func() error, error) {
	if err := key.validate(); err != nil {
		return nil, err
	}
	releaseUser, err := journal.lockMode(ctx, key.userID, syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	releaseDesktop, err := journal.lock(ctx, key.name())
	if err != nil {
		return nil, errors.Join(err, releaseUser())
	}
	return func() error { return errors.Join(releaseDesktop(), releaseUser()) }, nil
}

type sessionRecord struct {
	Version int                     `json:"version"`
	Command computer.SessionCommand `json:"command"`
	Epoch   string                  `json:"epoch"`
	Phase   string                  `json:"phase"`
	// Deadline 仅用于审计；活动租约使用进程内单调时钟，重启必须重新清理。
	Deadline time.Time               `json:"deadline"`
	Group    *processgroup.Reference `json:"group,omitempty"`
}

func (journal *journal) readSession(key sessionKey) (*sessionRecord, error) {
	if err := key.validate(); err != nil {
		return nil, err
	}
	encoded, err := journal.readFile(key.name() + ".json")
	if err != nil || encoded == nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var state sessionRecord
	if err = decoder.Decode(&state); err != nil {
		return nil, errors.New("电脑执行记录格式无效")
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("电脑执行记录包含多份内容")
	}
	if err = state.validate(key); err != nil {
		return nil, err
	}
	return &state, nil
}

func (state *sessionRecord) validate(key sessionKey) error {
	if state == nil || state.Version != 2 || state.Command.Validate() != nil || sessionKeyOf(state.Command.Session) != key || state.Deadline.IsZero() {
		return errors.New("电脑执行记录身份无效")
	}
	if _, err := ulid.ParseStrict(state.Epoch); err != nil {
		return errors.New("电脑执行记录进程代次无效")
	}
	switch state.Phase {
	case "opening", "active", "closing", "quarantined", "closed":
	default:
		return errors.New("电脑执行记录状态无效")
	}
	if state.Group != nil && (state.Group.Validate() != nil || state.Group.Session != state.Command.Session) || state.Phase == "active" && state.Group == nil {
		return errors.New("电脑执行记录资源组身份无效")
	}
	return nil
}
func (journal *journal) saveSession(state *sessionRecord) error {
	if state == nil {
		return errors.New("电脑执行记录缺失")
	}
	key := sessionKeyOf(state.Command.Session)
	if err := state.validate(key); err != nil {
		return err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return errors.New("编码电脑执行记录失败")
	}
	return journal.writeFile(key.name()+".json", encoded)
}

// visitSessions 流式遍历桌面记录；旧用户级文件与无效名称明确失败，不转换或丢弃关闭证据。
// 生命周期调用持有用户独占锁；启动恢复持有节点执行服务锁。
func (journal *journal) visitSessions(ctx context.Context, userID string, visit func(sessionKey) error) (result error) {
	directory, err := journal.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, directory.Close()) }()
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(100)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, entry := range entries {
			if err = ctx.Err(); err != nil {
				return err
			}
			if !strings.HasSuffix(entry.Name(), ".session.json") {
				continue
			}
			if userID != "" && !strings.HasPrefix(entry.Name(), userID+".") {
				continue
			}
			parts := strings.Split(strings.TrimSuffix(entry.Name(), ".session.json"), ".")
			if len(parts) != 2 {
				return errors.New("电脑执行记录文件名无效")
			}
			key := sessionKey{parts[0], parts[1]}
			if err = key.validate(); err != nil {
				return err
			}
			if err = visit(key); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

// recover 不按墙上时间恢复旧执行权，每个桌面的未知结果都必须先确认清理。
func (controller *SessionController) recover(ctx context.Context) error {
	return controller.node.journal.visitSessions(ctx, "", func(key sessionKey) error {
		_, err := controller.reapDesktop(ctx, key, true)
		return err
	})
}

func (journal *journal) requireClosedSessions(ctx context.Context, userID string) error {
	return journal.visitSessions(ctx, userID, func(key sessionKey) error {
		state, err := journal.readSession(key)
		if err != nil {
			return err
		}
		if state == nil || state.Phase != "closed" {
			return failure("computer_session_active")
		}
		return nil
	})
}
