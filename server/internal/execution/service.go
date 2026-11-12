package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/modelusage"
	"io.yueyang/tom/internal/store/postgres"
)

var (
	ErrInvalidInput     = errors.New("消息或运行参数无效")
	ErrNotFound         = errors.New("消息或运行不存在")
	ErrConflict         = errors.New("请求内容冲突或运行状态已变更")
	ErrBusy             = errors.New("会话已有未完成回合")
	ErrCapacity         = errors.New("当前用户的待执行任务已达到上限")
	ErrOperationLimit   = errors.New("运行操作数量已达到上限")
	ErrContextLimit     = errors.New("当前消息超过运行上下文大小限制")
	ErrUnavailable      = errors.New("用户、机器人或会话当前不可执行")
	ErrConversationKind = errors.New("此会话类型尚未接入新的执行链路")
	ErrNoWork           = errors.New("当前没有可领取的任务")
	ErrLeaseLost        = errors.New("运行租约已失效")
	ErrCanceled         = errors.New("运行已请求取消")
	ErrUncertain        = errors.New("外部操作结果尚不确定，不能重复执行或报告成功")
	ErrComputerBusy     = errors.New("电脑会话尚未确认释放")
	ErrComputerLost     = errors.New("电脑会话已因失效终止，不能报告成功")
	ErrImageCapacity    = errors.New("用户截图存储已达到上限")
)

// Resolver 仅根据持久任务所属用户与机器人选择模型，不接受客户端覆盖。
type Resolver interface {
	Resolve(context.Context, string, string) (modelregistry.Selection, error)
}
type Options struct {
	LeaseLifetime   time.Duration
	RecoveryDelay   time.Duration
	MaxAttempts     int
	MaxOperations   int
	ContextMessages int
	ContextBytes    int
	Images          ImageOptions
}
type Service struct {
	tenant   *postgres.Database
	dispatch *postgres.Database
	resolver Resolver
	options  Options
	meter    *modelusage.Service
}

// New 分离业务与调度连接，拒绝无限租约、无界恢复和缺失模型策略解析器。
func New(tenant, dispatch *postgres.Database, resolver Resolver, options Options) (*Service, error) {
	if tenant == nil || tenant.Role() != postgres.TenantRole || dispatch == nil || dispatch.Role() != postgres.DispatchRole || resolver == nil ||
		options.LeaseLifetime < time.Second || options.LeaseLifetime > 5*time.Minute || options.RecoveryDelay < time.Millisecond || options.RecoveryDelay > time.Minute ||
		options.MaxAttempts < 1 || options.MaxAttempts > 100 || options.MaxOperations < 1 || options.MaxOperations > 512 || options.ContextMessages < 1 || options.ContextMessages > 200 || options.ContextBytes < 1024 || options.ContextBytes > 1<<20 || options.Images.Validate() != nil {
		return nil, ErrInvalidInput
	}
	meter, err := modelusage.New(tenant, dispatch)
	if err != nil {
		return nil, err
	}
	return &Service{tenant: tenant, dispatch: dispatch, resolver: resolver, options: options, meter: meter}, nil
}
func validID(value string) bool { _, err := ulid.ParseStrict(value); return err == nil }

var codePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
var stepPattern = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,119}$`)

func validText(value string, maxBytes int) bool {
	return utf8.ValidString(value) && len(value) <= maxBytes && strings.TrimSpace(value) != "" && !strings.ContainsFunc(value, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r' })
}
func (service *Service) user(ctx context.Context, principal identity.Principal, write bool, apply func(*sql.Tx) error) error {
	if !validID(principal.UserID) {
		return identity.ErrInvalidSession
	}
	operation := func(transaction *sql.Tx) error {
		if err := identity.CheckTenantSession(ctx, transaction, principal); err != nil {
			return err
		}
		return apply(transaction)
	}
	if write {
		return translate(service.tenant.Tenant(ctx, principal.UserID, operation))
	}
	return translate(service.tenant.Snapshot(ctx, principal.UserID, operation))
}
func translate(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{ErrInvalidInput, ErrNotFound, ErrConflict, ErrBusy, ErrCapacity, ErrImageCapacity, ErrOperationLimit, ErrContextLimit, ErrUnavailable, ErrConversationKind, ErrNoWork, ErrLeaseLost, ErrCanceled, ErrUncertain, ErrComputerBusy, ErrComputerLost, modelusage.ErrInvalidInput, modelusage.ErrQuota, modelusage.ErrConcurrency, modelusage.ErrConflict, modelusage.ErrNotFound, identity.ErrInvalidSession, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var failure interface{ SQLState() string }
	if errors.As(err, &failure) {
		switch failure.SQLState() {
		case "23505", "40001", "40P01":
			return ErrConflict
		case "23503":
			return ErrNotFound
		}
		return fmt.Errorf("运行事务失败（SQLSTATE %s）", failure.SQLState())
	}
	return errors.New("运行事务失败")
}
func event(ctx context.Context, transaction *sql.Tx, userID, kind, id string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = postgres.AppendEvent(ctx, transaction, userID, kind, id, encoded)
	return err
}

type scanner interface{ Scan(...any) error }

const runColumns = `id,turn_id,conversation_id,bot_id,status,error_code,created_at,started_at,finished_at,
	COALESCE((SELECT CASE WHEN s.status='starting' THEN 'starting_desktop' ELSE s.wait_reason END FROM tombot.computer_sessions s
	WHERE s.user_id=runs.user_id AND s.run_id=runs.id AND s.status IN ('waiting','starting') ORDER BY s.ticket DESC LIMIT 1),'')`

func scanRun(row scanner) (Run, error) {
	var run Run
	err := row.Scan(&run.ID, &run.TurnID, &run.ConversationID, &run.BotID, &run.Status, &run.ErrorCode, &run.CreatedAt, &run.StartedAt, &run.FinishedAt, &run.ComputerWaitReason)
	return run, err
}
func readRun(ctx context.Context, transaction *sql.Tx, userID, id string) (Run, error) {
	return scanRun(transaction.QueryRowContext(ctx, `SELECT `+runColumns+` FROM tombot.runs WHERE user_id=$1 AND id=$2`, userID, id))
}
func isTerminal(status string) bool {
	return status == "succeeded" || status == "failed" || status == "canceled" || status == "unknown"
}
