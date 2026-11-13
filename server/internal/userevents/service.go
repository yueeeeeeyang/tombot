// Package userevents 读取当前用户已提交的事件，供客户端按游标增量同步。
// 事件只用于提示客户端重新读取资源，不承载授权，也不能替代快照水位。
package userevents

import (
	"context"
	"database/sql"
	"errors"

	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/store/postgres"
)

// 单次读取上限与事件表的物理容量无关，只限制一次响应的大小。
const (
	DefaultBatch = 200
	MaxBatch     = 500
)

var (
	ErrInvalidInput = errors.New("事件游标或数量无效")
	ErrUnavailable  = errors.New("事件读取暂不可用")
)

// Event 与存储层共用同一结构，避免两处定义在字段或 JSON 名称上分叉。
type Event = postgres.Event

type Service struct{ database *postgres.Database }

// New 只接受租户连接；跨用户读取事件必须由各自身份进入，不使用控制连接。
func New(database *postgres.Database) (*Service, error) {
	if database == nil || database.Role() != postgres.TenantRole {
		return nil, errors.New("事件服务需要租户数据库连接")
	}
	return &Service{database: database}, nil
}

// Read 在一次只读快照中复核会话并返回 after 之后的事件。
// 会话失效立即返回身份错误，不能继续向已撤销的连接推送内容。
// 游标早于清理边界时返回 ErrCursorExpired：中间事件已被删除，续订方必须重新读取快照。
func (service *Service) Read(ctx context.Context, principal identity.Principal, after int64, limit int) ([]Event, error) {
	if after < 0 || limit < 1 || limit > MaxBatch {
		return nil, ErrInvalidInput
	}
	var events []Event
	err := service.database.Snapshot(ctx, principal.UserID, func(transaction *sql.Tx) error {
		if err := identity.CheckTenantSession(ctx, transaction, principal); err != nil {
			return err
		}
		prunedThrough, err := postgres.PrunedThrough(ctx, transaction, principal.UserID)
		if err != nil {
			return err
		}
		if after < prunedThrough {
			return ErrCursorExpired
		}
		var readErr error
		events, readErr = postgres.ReadEvents(ctx, transaction, principal.UserID, after, limit)
		return readErr
	})
	if err != nil {
		return nil, translate(err)
	}
	return events, nil
}

func translate(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{ErrInvalidInput, ErrCursorExpired, identity.ErrInvalidSession, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrUnavailable
}
