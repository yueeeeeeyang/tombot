// Package accounts 管理平台账户与同步电脑开户，不读取用户业务内容。
package accounts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/store/postgres"
)

var (
	ErrInvalidInput          = errors.New("用户创建参数无效")
	ErrConflict              = errors.New("用户名或幂等键与已有请求冲突")
	ErrNoCapacity            = errors.New("电脑节点尚未就绪或容量不足")
	ErrImageUnavailable      = errors.New("电脑镜像不存在或未启用")
	ErrNotFound              = errors.New("用户创建操作不存在")
	ErrPending               = errors.New("用户创建等待超时，请查询同一操作的进度")
	ErrProvisionFailed       = errors.New("电脑创建失败，用户尚不可登录")
	ErrQuarantined           = errors.New("电脑资源状态无法确认，已隔离且未开放账户")
	ErrLeaseLost             = errors.New("用户创建操作租约已失效")
	ErrAlreadyInitialized    = errors.New("平台已存在用户，不能重复初始化首位管理员")
	ErrActivationUnavailable = errors.New("用户尚未完成电脑创建或已经激活")
)

// Authorizer 由身份模块校验管理员和二次验证，不接受 HTTP 自行声明的角色。
type Authorizer interface {
	RequireAdmin(context.Context, identity.Principal, bool) error
}

// Options 控制工作者、节点调用、等待和重试上限，所有时限由部署配置明确提供。
type Options struct {
	WorkerID           string
	Workers            int
	CallTimeout        time.Duration
	LeaseDuration      time.Duration
	OperationTimeout   time.Duration
	RequestWait        time.Duration
	PollInterval       time.Duration
	RetryDelay         time.Duration
	MaxReconciliations int
	ActivationLifetime time.Duration
	ModelConcurrency   int
	DesktopConcurrency int
	QueuedRuns         int
}

// Service 将短数据库事务与外部节点调用分离；Run 的上下文独立于创建请求的连接。
type Service struct {
	database   *postgres.Database
	runtime    computer.Runtime
	authorizer Authorizer
	options    Options
}

// CreateRequest 固定本次开户的资源和身份；重试同一幂等键时所有业务字段必须一致。
type CreateRequest struct {
	Username       string             `json:"username"`
	DisplayName    string             `json:"display_name"`
	Role           string             `json:"role"`
	NodeID         string             `json:"node_id"`
	ImageID        string             `json:"image_id"`
	Resources      computer.Resources `json:"resources"`
	IdempotencyKey string             `json:"idempotency_key"`
	RequestID      string             `json:"-"`
}

// Operation 为可展示进度，不含激活值、节点地址或其他用户内容。
type Operation struct {
	ID         string    `json:"id"`
	UserID     string    `json:"user_id"`
	ComputerID string    `json:"computer_id"`
	Status     string    `json:"status"`
	Stage      string    `json:"stage"`
	Attempt    int       `json:"attempt"`
	ErrorCode  *string   `json:"error_code,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// New 拒绝不匹配的数据库用途和无界配置；调用方在开启用户创建前启动 Run。
func New(database *postgres.Database, runtime computer.Runtime, authorizer Authorizer, options Options) (*Service, error) {
	if database == nil || database.Role() != postgres.ControlRole || runtime == nil || authorizer == nil {
		return nil, errors.New("用户管理依赖配置无效")
	}
	if _, err := ulid.ParseStrict(options.WorkerID); err != nil {
		return nil, errors.New("开户工作者标识无效")
	}
	if options.Workers < 1 || options.Workers > 32 || options.CallTimeout < time.Second || options.CallTimeout > 5*time.Minute || options.LeaseDuration < 3*options.CallTimeout || options.LeaseDuration > 30*time.Minute ||
		options.OperationTimeout < options.LeaseDuration || options.OperationTimeout > time.Hour || options.RequestWait < time.Millisecond || options.RequestWait > options.OperationTimeout ||
		options.PollInterval < time.Millisecond || options.PollInterval > time.Second || options.RetryDelay < time.Millisecond || options.RetryDelay > time.Minute || options.MaxReconciliations < 1 || options.MaxReconciliations > 20 ||
		options.ActivationLifetime < time.Minute || options.ActivationLifetime > 24*time.Hour || options.ModelConcurrency < 1 || options.ModelConcurrency > 100 || options.DesktopConcurrency < 1 || options.DesktopConcurrency > computer.MaxConcurrentDesktops || options.QueuedRuns < 1 || options.QueuedRuns > 10000 {
		return nil, errors.New("用户管理超时或资源限额无效")
	}
	return &Service{database: database, runtime: runtime, authorizer: authorizer, options: options}, nil
}

func (service *Service) transaction(ctx context.Context, apply func(*sql.Tx) error) error {
	err := service.database.Platform(ctx, apply)
	if err == nil {
		return nil
	}
	for _, known := range []error{ErrInvalidInput, ErrConflict, ErrNoCapacity, ErrImageUnavailable, ErrNotFound, ErrLeaseLost, ErrAlreadyInitialized, ErrActivationUnavailable, ErrQuarantined, context.Canceled, context.DeadlineExceeded} {
		if err == known {
			return known
		}
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		if state.SQLState() == "23505" {
			return ErrConflict
		}
		return fmt.Errorf("用户管理数据库操作失败（SQLSTATE %s）", state.SQLState())
	}
	return errors.New("用户管理数据库操作失败")
}

// Create 只在同一用户电脑已就绪且状态提交成功时返回成功；请求取消不会取消持久操作。
func (service *Service) Create(ctx context.Context, actor identity.Principal, request CreateRequest) (Operation, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, true); err != nil {
		return Operation{}, err
	}
	operation, err := service.reserve(ctx, actor.UserID, request, false)
	if err != nil {
		return operation, err
	}
	return service.wait(ctx, operation)
}

// Bootstrap 仅供受保护的本地部署命令使用；全库互斥检查确保只能建立首位管理员。
// 首位管理员也走相同电脑供给与等待过程，不存在免电脑的初始化账户。
func (service *Service) Bootstrap(ctx context.Context, request CreateRequest) (Operation, error) {
	request.Role = "admin"
	operation, err := service.reserve(ctx, "", request, true)
	if err != nil {
		return operation, err
	}
	return service.wait(ctx, operation)
}

func (service *Service) wait(ctx context.Context, operation Operation) (Operation, error) {
	ctx, cancel := context.WithTimeout(ctx, service.options.RequestWait)
	defer cancel()
	ticker := time.NewTicker(service.options.PollInterval)
	defer ticker.Stop()
	for {
		current, err := service.readOperation(ctx, operation.ID)
		if err != nil {
			if ctx.Err() != nil {
				return operation, ErrPending
			}
			return operation, err
		}
		operation = current
		switch operation.Status {
		case "succeeded":
			return operation, nil
		case "failed":
			return operation, ErrProvisionFailed
		case "quarantined":
			return operation, ErrQuarantined
		}
		select {
		case <-ctx.Done():
			return operation, ErrPending
		case <-ticker.C:
		}
	}
}

// Get 供管理员查询同一持久操作，等待超时或客户端重连不会创建第二个账户。
func (service *Service) Get(ctx context.Context, actor identity.Principal, operationID string) (Operation, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, false); err != nil {
		return Operation{}, err
	}
	return service.readOperation(ctx, operationID)
}

// GetBootstrap 仅供本机部署查询首位管理员的持久操作；不暴露普通开户或激活材料。
func (service *Service) GetBootstrap(ctx context.Context, operationID string) (Operation, error) {
	if _, err := ulid.ParseStrict(operationID); err != nil {
		return Operation{}, ErrInvalidInput
	}
	var operation Operation
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		err := transaction.QueryRowContext(ctx, `SELECT o.id,o.user_id,o.computer_id,o.status,o.stage,o.attempt,o.error_code,o.created_at,o.updated_at
			FROM tombot.user_creation_operations o JOIN tombot.users u ON u.id=o.user_id
			WHERE o.id=$1 AND o.actor_id IS NULL AND u.role='admin'`, operationID).
			Scan(&operation.ID, &operation.UserID, &operation.ComputerID, &operation.Status, &operation.Stage, &operation.Attempt, &operation.ErrorCode, &operation.CreatedAt, &operation.UpdatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return operation, err
}

func (service *Service) readOperation(ctx context.Context, id string) (Operation, error) {
	if _, err := ulid.ParseStrict(id); err != nil {
		return Operation{}, ErrInvalidInput
	}
	var operation Operation
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		err := transaction.QueryRowContext(ctx, `SELECT id,user_id,computer_id,status,stage,attempt,error_code,created_at,updated_at FROM tombot.user_creation_operations WHERE id=$1`, id).
			Scan(&operation.ID, &operation.UserID, &operation.ComputerID, &operation.Status, &operation.Stage, &operation.Attempt, &operation.ErrorCode, &operation.CreatedAt, &operation.UpdatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return operation, err
}
