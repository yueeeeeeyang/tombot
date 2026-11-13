// Package modelregistry 管理平台唯一模型目录、凭据版本与已发布策略。
package modelregistry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
)

var (
	ErrInvalidInput = errors.New("模型配置参数无效")
	ErrNotFound     = errors.New("模型配置不存在")
	ErrConflict     = errors.New("模型配置已变更，请重新读取")
	ErrUnavailable  = errors.New("管理员尚未配置可用模型，或所选策略已停用")
	ErrCredential   = errors.New("模型凭据不可用")
	ErrVerification = errors.New("模型连通性或声明能力验证失败")
)

// Authorizer 复用身份服务的当前管理员和近期二次校验，禁止依赖请求自报角色。
type Authorizer interface {
	RequireAdmin(context.Context, identity.Principal, bool) error
}

// Verifier 必须实际验证文本及配置声明的工具、视觉能力；不能仅检查地址或目录响应。
// 调用不持有数据库事务，不执行用户工具，不保留凭据；失败不得回传上游响应正文。
type Verifier interface {
	Verify(context.Context, Configuration, []byte) error
}

// Options 指定管理员验证调用上限；同一发布请求不会自动重试上游调用。
type Options struct{ VerificationTimeout time.Duration }

// Service 将控制目录与租户所属权检查分离，不能用管理连接读取聊天内容。
type Service struct {
	control    *postgres.Database
	tenant     *postgres.Database
	authorizer Authorizer
	keys       *Keyring
	verifier   Verifier
	discoverer Discoverer
	options    Options
}

// New 拒绝缺失的密钥、验证器或错误权限连接；不会生成临时主密钥或跳过能力验证。
// 发现器与验证器共用同一受控出口网关，二者都可为空以外的同一实现。
func New(control, tenant *postgres.Database, authorizer Authorizer, keys *Keyring, verifier Verifier, discoverer Discoverer, options Options) (*Service, error) {
	if control == nil || control.Role() != postgres.ControlRole || tenant == nil || tenant.Role() != postgres.TenantRole ||
		authorizer == nil || keys == nil || len(keys.keys) == 0 || verifier == nil || discoverer == nil ||
		options.VerificationTimeout < time.Second || options.VerificationTimeout > time.Minute {
		return nil, errors.New("模型目录依赖或验证时限无效")
	}
	return &Service{control: control, tenant: tenant, authorizer: authorizer, keys: keys, verifier: verifier, discoverer: discoverer, options: options}, nil
}

func validID(value string) bool { _, err := ulid.ParseStrict(value); return err == nil }
func validText(value string, maximum int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) == value && len(value) > 0 && utf8.RuneCountInString(value) <= maximum &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

// 地址结构先在入库时验证；连接时仍必须重新解析并校验实际目标地址和部署允许范围。
func validURL(value string) bool {
	if len(value) > 2048 || strings.ContainsAny(value, "\\\r\n\t?#") {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}
	if parsed.Port() != "" {
		port, err := strconv.Atoi(parsed.Port())
		if err != nil || port < 1 || port > 65535 {
			return false
		}
	}
	return err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" && parsed.User == nil && parsed.Opaque == "" &&
		parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == "" && parsed.RawFragment == "" &&
		!strings.Contains(parsed.Hostname(), "%") && strings.TrimSpace(value) == value
}

func (service *Service) authorize(ctx context.Context, actor identity.Principal, requestID string) error {
	if !validID(requestID) {
		return ErrInvalidInput
	}
	return service.authorizer.RequireAdmin(ctx, actor, true)
}

func (service *Service) transaction(ctx context.Context, apply func(*sql.Tx) error) error {
	err := service.control.Platform(ctx, apply)
	for _, known := range []error{ErrInvalidInput, ErrNotFound, ErrConflict, ErrUnavailable, ErrCredential, ErrVerification, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	if err == nil {
		return nil
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		if state.SQLState() == "23505" || state.SQLState() == "40001" || state.SQLState() == "40P01" {
			return ErrConflict
		}
		if state.SQLState() == "23503" {
			return ErrNotFound
		}
		return fmt.Errorf("模型目录事务失败（SQLSTATE %s）", state.SQLState())
	}
	return errors.New("模型目录事务失败")
}

func audit(ctx context.Context, transaction *sql.Tx, actor identity.Principal, requestID, action, target string, changes any) error {
	encoded, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	_, err = transaction.ExecContext(ctx, `INSERT INTO tombot.admin_audit_events(id,actor_id,action,target_kind,target_id,request_id,result,changes)
		VALUES($1,$2,$3,'model_configuration',$4,$5,'succeeded',$6)`, ids.New(), actor.UserID, action, target, requestID, encoded)
	return err
}

func requireRow(row *sql.Row) error {
	var id string
	err := row.Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
