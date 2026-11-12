// Package computercontrol 持久管理共享电脑内各桌面的执行权；节点确认回收之前不允许同桌面交接。
package computercontrol

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/store/postgres"
)

var (
	ErrInvalidInput   = errors.New("电脑会话参数无效")
	ErrLeaseLost      = errors.New("任务或电脑执行权已失效")
	ErrWaiting        = errors.New("正在等待电脑执行权")
	ErrCapacity       = errors.New("当前用户的电脑等待队列已满")
	ErrUnavailable    = errors.New("电脑执行会话当前不可用")
	ErrNode           = errors.New("电脑节点尚未确认执行状态")
	ErrCleanupBlocked = errors.New("电脑清理未确认，保持隔离")
)

// RunReference 仅由受信 Worker 从已领取任务构造，不接受客户端或模型指定的用户身份。
type RunReference struct {
	UserID, JobID, RunID, BotID, OwnerID string
	FencingToken                         int64
	Attempt                              int
}

func (reference RunReference) valid() bool {
	for _, id := range []string{reference.UserID, reference.JobID, reference.RunID, reference.BotID, reference.OwnerID} {
		if _, err := ulid.ParseStrict(id); err != nil {
			return false
		}
	}
	return reference.FencingToken > 0 && reference.Attempt > 0
}

type Options struct {
	LeaseLifetime, WaitTimeout, HoldTimeout, NodeTimeout, RetryDelay time.Duration
	MaxWaiting, MaxCleanupAttempts                                   int
}
type Service struct {
	tenant, dispatch *postgres.Database
	node             computer.SessionRuntime
	options          Options
	logger           *slog.Logger
}

// New 需要真实节点适配器与分离的业务、调度连接；不提供默认成功的清理实现。
func New(tenant, dispatch *postgres.Database, node computer.SessionRuntime, options Options, logger *slog.Logger) (*Service, error) {
	if tenant == nil || tenant.Role() != postgres.TenantRole || dispatch == nil || dispatch.Role() != postgres.DispatchRole || node == nil || logger == nil ||
		options.LeaseLifetime < time.Second || options.LeaseLifetime > 5*time.Minute || options.WaitTimeout < time.Second || options.WaitTimeout > time.Hour ||
		options.HoldTimeout < options.LeaseLifetime || options.HoldTimeout > 24*time.Hour || options.NodeTimeout < 50*time.Millisecond || options.NodeTimeout > 30*time.Second || options.NodeTimeout > options.LeaseLifetime/2 ||
		options.RetryDelay < 100*time.Millisecond || options.RetryDelay > time.Minute || options.MaxWaiting < 1 || options.MaxWaiting > 1000 || options.MaxCleanupAttempts < 1 || options.MaxCleanupAttempts > 10 {
		return nil, ErrInvalidInput
	}
	return &Service{tenant: tenant, dispatch: dispatch, node: node, options: options, logger: logger}, nil
}

func (service *Service) HeartbeatInterval() time.Duration { return service.options.LeaseLifetime / 3 }

type record struct {
	computer.ExecutionSession
	Reference                   RunReference
	State                       string
	WaitReason                  string
	ErrorCode                   string
	Revision                    int64
	Ticket                      int64
	WaitDeadline                time.Time
	HoldDeadline, LeaseDeadline sql.NullTime
	CleanupAttempt              int
}

const columns = `user_id,id,computer_id,node_id,container_id,generation,run_id,bot_id,desktop_id,linux_uid,shared_gid,display_number,COALESCE(fencing_token,0),job_id,job_owner,job_fencing_token,job_attempt,status,wait_reason,error_code,revision,ticket,wait_deadline_at,hold_deadline_at,lease_expires_at,cleanup_attempt`

type scanner interface{ Scan(...any) error }

func scan(row scanner) (record, error) {
	var value record
	err := row.Scan(&value.UserID, &value.SessionID, &value.ComputerID, &value.NodeID, &value.ContainerID, &value.Generation, &value.RunID, &value.BotID, &value.DesktopID, &value.UID, &value.GID, &value.DisplayNumber, &value.FencingToken,
		&value.Reference.JobID, &value.Reference.OwnerID, &value.Reference.FencingToken, &value.Reference.Attempt, &value.State, &value.WaitReason, &value.ErrorCode, &value.Revision, &value.Ticket, &value.WaitDeadline, &value.HoldDeadline, &value.LeaseDeadline, &value.CleanupAttempt)
	value.Reference.UserID, value.Reference.RunID, value.Reference.BotID = value.UserID, value.RunID, value.BotID
	return value, err
}
func read(ctx context.Context, tx *sql.Tx, reference RunReference) (record, error) {
	return scan(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM tombot.computer_sessions WHERE user_id=$1 AND run_id=$2 AND job_fencing_token=$3 AND job_attempt=$4 AND job_owner=$5`, reference.UserID, reference.RunID, reference.FencingToken, reference.Attempt, reference.OwnerID))
}

// lockJob 是所有电脑事务的第一个锁；随后锁用户电脑头，再修改会话，避免与运行完成产生反向锁序。
func lockJob(ctx context.Context, tx *sql.Tx, reference RunReference) (bool, error) {
	var live bool
	err := tx.QueryRowContext(ctx, `SELECT status='leased' AND lease_owner=$5 AND fencing_token=$6 AND attempt=$7 AND lease_expires_at>clock_timestamp() AND cancel_requested_at IS NULL
		FROM tombot.jobs WHERE user_id=$1 AND id=$2 AND run_id=$3 AND bot_id=$4 FOR UPDATE`, reference.UserID, reference.JobID, reference.RunID, reference.BotID, reference.OwnerID, reference.FencingToken, reference.Attempt).Scan(&live)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrLeaseLost
	}
	if err != nil {
		return false, err
	}
	if !live {
		return false, nil
	}
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.users u JOIN tombot.bots b ON b.user_id=u.id JOIN tombot.jobs j ON j.user_id=u.id
		JOIN tombot.conversations c ON c.user_id=j.user_id AND c.id=j.conversation_id
		WHERE u.id=$1 AND b.id=$2 AND j.id=$3 AND u.status='active' AND NOT b.is_archived AND NOT c.is_archived)`, reference.UserID, reference.BotID, reference.JobID).Scan(&live)
	return live, err
}
func lockHead(ctx context.Context, tx *sql.Tx, userID string) error {
	var id string
	return tx.QueryRowContext(ctx, `SELECT computer_id FROM tombot.computer_execution_heads WHERE user_id=$1 FOR UPDATE`, userID).Scan(&id)
}
func now(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var at time.Time
	err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&at)
	return at, err
}
func sessionEvent(ctx context.Context, tx *sql.Tx, value record, state, code string) error {
	payload, err := json.Marshal(map[string]string{"computer_id": value.ComputerID, "desktop_id": value.DesktopID, "run_id": value.RunID, "bot_id": value.BotID, "state": state, "wait_reason": value.WaitReason, "error_code": code})
	if err != nil {
		return err
	}
	_, err = postgres.AppendEvent(ctx, tx, value.UserID, "computer.session.updated", value.SessionID, payload)
	return err
}
func safe(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{ErrInvalidInput, ErrLeaseLost, ErrWaiting, ErrCapacity, ErrUnavailable, ErrNode, ErrCleanupBlocked, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	var failure interface{ SQLState() string }
	if errors.As(err, &failure) {
		return fmt.Errorf("电脑会话事务失败（SQLSTATE %s）", failure.SQLState())
	}
	return errors.New("电脑会话事务失败")
}
