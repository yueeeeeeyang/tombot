package bots

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"golang.org/x/text/unicode/norm"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/store/postgres"
)

var (
	ErrInvalidInput = errors.New("机器人或群组参数无效")
	ErrNotFound     = errors.New("资源不存在")
	ErrConflict     = errors.New("资源已变更或名称冲突，请重新读取")
	ErrCapacity     = errors.New("机器人或群组数量已达到上限")
	ErrInviteDenied = errors.New("机器人已归档或不接受群组邀请")
	ErrArchived     = errors.New("资源已归档，请先恢复")
)

// Options 由部署设置目录容量，工具名称来自运行工具注册表，不由请求扩充。
type Options struct {
	MaxBots   int
	MaxGroups int
	ToolNames []string
}

type Service struct {
	database *postgres.Database
	options  Options
	tools    map[string]bool
}

var referencePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,119}$`)
var toolPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ValidToolName 校验目录和执行器共同使用的稳定工具名，避免允许范围随入口变化。
func ValidToolName(name string) bool { return toolPattern.MatchString(name) }

// New 要求租户连接与有限容量，不接受能跨用户读取的控制连接。
func New(database *postgres.Database, options Options) (*Service, error) {
	if database == nil || database.Role() != postgres.TenantRole || options.MaxBots < 1 || options.MaxBots > computer.MaxDesktops || options.MaxGroups < 1 || options.MaxGroups > 10000 || len(options.ToolNames) > 128 {
		return nil, ErrInvalidInput
	}
	known := make(map[string]bool, len(options.ToolNames))
	for _, name := range options.ToolNames {
		if !ValidToolName(name) || known[name] {
			return nil, ErrInvalidInput
		}
		known[name] = true
	}
	options.ToolNames = slices.Clone(options.ToolNames)
	return &Service{database, options, known}, nil
}

func validID(value string) bool { _, err := ulid.ParseStrict(value); return err == nil }
func validPage(before string, limit int) bool {
	return (before == "" || validID(before)) && limit >= 1 && limit <= 100
}

func text(value string, minimum, maximum int, multiline bool) (string, error) {
	if !utf8.ValidString(value) {
		return "", ErrInvalidInput
	}
	value = norm.NFC.String(strings.TrimSpace(value))
	if len(value) < minimum || len(value) > maximum || strings.ContainsFunc(value, func(character rune) bool {
		return unicode.IsControl(character) && !(multiline && (character == '\n' || character == '\t'))
	}) {
		return "", ErrInvalidInput
	}
	return value, nil
}

func (service *Service) normalize(spec Spec) (Spec, error) {
	var err error
	for _, field := range []struct {
		value            *string
		minimum, maximum int
		multiline        bool
	}{
		{&spec.Name, 1, 120, false}, {&spec.Description, 0, 8192, true}, {&spec.Category, 0, 120, false}, {&spec.SystemPrompt, 1, 32768, true},
	} {
		*field.value, err = text(*field.value, field.minimum, field.maximum, field.multiline)
		if err != nil {
			return Spec{}, err
		}
	}
	if spec.AvatarRef != "" && !referencePattern.MatchString(spec.AvatarRef) || spec.AccentHue < 0 || spec.AccentHue > 359 ||
		(spec.ApprovalMode != "loose" && spec.ApprovalMode != "standard" && spec.ApprovalMode != "strict") || spec.ToolPermissions == nil || len(spec.ToolPermissions) > 128 {
		return Spec{}, ErrInvalidInput
	}
	spec.ToolPermissions = slices.Clone(spec.ToolPermissions)
	slices.Sort(spec.ToolPermissions)
	for index, name := range spec.ToolPermissions {
		if !service.tools[name] || index > 0 && spec.ToolPermissions[index-1] == name {
			return Spec{}, ErrInvalidInput
		}
	}
	return spec, nil
}

func normalizeGroup(spec GroupSpec) (GroupSpec, error) {
	var err error
	spec.Name, err = text(spec.Name, 1, 120, false)
	if err != nil {
		return GroupSpec{}, err
	}
	spec.Purpose, err = text(spec.Purpose, 0, 8192, true)
	if err != nil || len(spec.MemberIDs) < 1 || len(spec.MemberIDs) > 32 || spec.MaxCostMicro != nil && (*spec.MaxCostMicro < 1 || *spec.MaxCostMicro > 1_000_000_000_000) {
		return GroupSpec{}, ErrInvalidInput
	}
	spec.MemberIDs = slices.Clone(spec.MemberIDs)
	slices.Sort(spec.MemberIDs)
	for index, id := range spec.MemberIDs {
		if !validID(id) || index > 0 && id == spec.MemberIDs[index-1] {
			return GroupSpec{}, ErrInvalidInput
		}
	}
	if spec.MaxCostMicro != nil {
		copied := *spec.MaxCostMicro
		spec.MaxCostMicro = &copied
	}
	return spec, nil
}

func requestDigest(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	return digest[:], nil
}

// transaction 把身份检查和业务放在同一个用户事务；写操作串行化低频目录变更。
// 模型调用与消息写入不会持有此目录锁，不能把它用于电脑执行互斥。
func (service *Service) transaction(ctx context.Context, principal identity.Principal, write bool, apply func(*sql.Tx) error) error {
	if !validID(principal.UserID) {
		return identity.ErrInvalidSession
	}
	operation := func(transaction *sql.Tx) error {
		if err := identity.CheckTenantSession(ctx, transaction, principal); err != nil {
			return err
		}
		if write {
			if _, err := transaction.ExecContext(ctx, `INSERT INTO tombot.user_workspaces(user_id) VALUES($1) ON CONFLICT(user_id) DO NOTHING`, principal.UserID); err != nil {
				return err
			}
			var revision int64
			if err := transaction.QueryRowContext(ctx, `SELECT revision FROM tombot.user_workspaces WHERE user_id=$1 FOR UPDATE`, principal.UserID).Scan(&revision); err != nil {
				return err
			}
		}
		return apply(transaction)
	}
	var err error
	if write {
		err = service.database.Tenant(ctx, principal.UserID, operation)
	} else {
		err = service.database.Snapshot(ctx, principal.UserID, operation)
	}
	if err == nil {
		return nil
	}
	for _, known := range []error{ErrInvalidInput, ErrNotFound, ErrConflict, ErrCapacity, ErrInviteDenied, ErrArchived, identity.ErrInvalidSession, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		switch state.SQLState() {
		case "23505", "40001", "40P01":
			return ErrConflict
		case "23503":
			return ErrNotFound
		}
		return fmt.Errorf("机器人目录事务失败（SQLSTATE %s）", state.SQLState())
	}
	return errors.New("机器人目录事务失败")
}

func emitChange(ctx context.Context, transaction *sql.Tx, userID, kind, id string, revision int64) error {
	payload, err := json.Marshal(struct {
		Revision int64 `json:"revision"`
	}{revision})
	if err != nil {
		return err
	}
	_, err = postgres.AppendEvent(ctx, transaction, userID, kind, id, payload)
	return err
}

type scanner interface{ Scan(...any) error }
