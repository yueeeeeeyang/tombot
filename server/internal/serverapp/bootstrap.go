package serverapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"time"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/accounts"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodeapi"
	"io.yueyang/tom/internal/store/postgres"
)

// BootstrapRequest 是本机部署操作，不注册 HTTP 路由，也不接受登录口令或激活值。
type BootstrapRequest struct {
	Action         string                  `json:"action"`
	Create         *accounts.CreateRequest `json:"create,omitempty"`
	OperationID    string                  `json:"operation_id,omitempty"`
	ActivationFile string                  `json:"activation_file,omitempty"`
}

// BootstrapResult 只包含可安全输出的回执；激活材料仅写入显式指定的私有文件。
type BootstrapResult struct {
	Action              string              `json:"action"`
	Operation           *accounts.Operation `json:"operation,omitempty"`
	ActivationUserID    string              `json:"activation_user_id,omitempty"`
	ActivationExpiresAt *time.Time          `json:"activation_expires_at,omitempty"`
	ActivationSaved     bool                `json:"activation_saved,omitempty"`
}

type bootstrapService interface {
	Bootstrap(context.Context, accounts.CreateRequest) (accounts.Operation, error)
	GetBootstrap(context.Context, string) (accounts.Operation, error)
	RetryBootstrap(context.Context, string, string) (accounts.Operation, error)
	ReconcileBootstrap(context.Context, string, string) (accounts.Operation, error)
	BootstrapActivation(context.Context, string, string) (accounts.Activation, error)
}

type localBootstrapAuthority struct{}

// RequireAdmin 封闭本机引导对象的普通管理路径；这里只能调用首位管理员的专用领域方法。
func (localBootstrapAuthority) RequireAdmin(context.Context, identity.Principal, bool) error {
	return identity.ErrForbidden
}

func (provision ProvisionConfig) options() accounts.Options {
	return accounts.Options{WorkerID: ids.New(), Workers: provision.Workers, CallTimeout: provision.CallTimeout.Value(), LeaseDuration: provision.LeaseDuration.Value(), OperationTimeout: provision.OperationTimeout.Value(), RequestWait: provision.RequestWait.Value(), PollInterval: provision.PollInterval.Value(), RetryDelay: provision.RetryDelay.Value(), MaxReconciliations: provision.MaxReconciliations, ActivationLifetime: provision.ActivationLifetime.Value(), ModelConcurrency: provision.ModelConcurrency, DesktopConcurrency: provision.DesktopConcurrency, QueuedRuns: provision.QueuedRuns}
}

// LoadBootstrapRequest 严格读取受保护的操作文件；未知字段、混合动作和多余内容全部拒绝。
func LoadBootstrapRequest(path string) (BootstrapRequest, error) {
	encoded, err := readFile(path, 16<<10, false)
	if err != nil {
		return BootstrapRequest{}, err
	}
	var request BootstrapRequest
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || request.validate() != nil {
		return BootstrapRequest{}, errors.New("首位管理员部署操作无效")
	}
	return request, nil
}

func (request BootstrapRequest) validate() error {
	if request.Action == "create" {
		if request.Create == nil || request.OperationID != "" || request.ActivationFile != "" || request.Create.Role != "" || request.Create.RequestID != "" {
			return errors.New("创建首位管理员只接受开户字段，角色固定为管理员")
		}
		return nil
	}
	if request.Create != nil {
		return errors.New("当前部署动作不接受开户字段")
	}
	if _, err := ulid.ParseStrict(request.OperationID); err != nil {
		return errors.New("部署操作标识无效")
	}
	switch request.Action {
	case "status", "retry", "reconcile":
		if request.ActivationFile == "" {
			return nil
		}
	case "activation":
		if filepath.IsAbs(request.ActivationFile) && filepath.Clean(request.ActivationFile) == request.ActivationFile && request.ActivationFile != "/" {
			return nil
		}
	}
	return errors.New("部署动作或激活文件路径无效")
}

// RunBootstrap 使用同一服务配置与最小控制角色；开户由已运行的 tomd 工作池处理。
// 请求结束不取消持久开户，超时返回原操作，后续只查询、对账或显式重试。
func RunBootstrap(ctx context.Context, configuration Config, request BootstrapRequest) (result BootstrapResult, failure error) {
	if err := configuration.validate(); err != nil {
		return result, err
	}
	if err := request.validate(); err != nil {
		return result, err
	}
	if request.Create != nil && request.Create.NodeID != configuration.Node.ID {
		return result, errors.New("首位管理员的节点必须与服务部署配置一致")
	}
	roles := []postgres.Role{postgres.ControlRole, postgres.IdentityRole, postgres.TenantRole, postgres.DispatchRole}
	connections, err := configuration.Database.connections(roles)
	if err != nil {
		return result, err
	}
	database, err := configuration.Database.open(ctx, postgres.ControlRole, connections[0])
	if err != nil {
		return result, err
	}
	defer func() { failure = errors.Join(failure, database.Close()) }()
	node, err := nodeapi.NewClient(nodeapi.ClientOptions{NodeID: configuration.Node.ID, Socket: configuration.Node.Socket, CallTimeout: configuration.Node.CallTimeout.Value()})
	if err != nil {
		return result, err
	}
	service, err := accounts.New(database, node, localBootstrapAuthority{}, configuration.Provision.options())
	if err != nil {
		return result, err
	}
	// 入口总时限还覆盖查询、对账及私有文件交付，不只限制开户等待。
	bounded, cancel := context.WithTimeout(ctx, configuration.HTTP.RequestTimeout.Value())
	defer cancel()
	return executeBootstrap(bounded, service, request)
}

func executeBootstrap(ctx context.Context, service bootstrapService, request BootstrapRequest) (result BootstrapResult, failure error) {
	result.Action = request.Action
	if err := request.validate(); err != nil {
		return result, err
	}
	if request.Action == "activation" {
		output, err := reserveActivationFile(request.ActivationFile)
		if err != nil {
			return result, err
		}
		defer func() { failure = errors.Join(failure, output.close()) }()
		activation, err := service.BootstrapActivation(ctx, request.OperationID, ids.New())
		if err != nil {
			return result, err
		}
		if err = output.save(activation); err != nil {
			return result, err
		}
		result.ActivationUserID = activation.UserID
		result.ActivationExpiresAt = &activation.ExpiresAt
		result.ActivationSaved = true
		return result, nil
	}
	var operation accounts.Operation
	switch request.Action {
	case "create":
		create := *request.Create
		create.RequestID = ids.New()
		operation, failure = service.Bootstrap(ctx, create)
	case "status":
		operation, failure = service.GetBootstrap(ctx, request.OperationID)
	case "retry":
		operation, failure = service.RetryBootstrap(ctx, request.OperationID, ids.New())
	case "reconcile":
		operation, failure = service.ReconcileBootstrap(ctx, request.OperationID, ids.New())
	}
	if operation.ID != "" {
		result.Operation = &operation
	}
	return result, failure
}
