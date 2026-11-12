package accounts

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/identity"
)

// UserSummary 仅供管理员查看账户和供给元数据，不包含会话、凭据或业务正文。
type UserSummary struct {
	ID                  string    `json:"id"`
	Username            string    `json:"username"`
	DisplayName         string    `json:"display_name"`
	Role                string    `json:"role"`
	Status              string    `json:"status"`
	ComputerStatus      *string   `json:"computer_status"`
	CreationOperationID *string   `json:"creation_operation_id"`
	CreatedAt           time.Time `json:"created_at"`
}

// NodeSummary 不返回节点连接地址；容量为查询时快照，最终准入仍由开户事务判断。
type NodeSummary struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Capacity int    `json:"capacity"`
	Reserved int    `json:"reserved"`
}

// ImageSummary 仅暴露可用于选择的固定镜像版本及启用状态。
type ImageSummary struct {
	ID        string `json:"id"`
	Reference string `json:"reference"`
	IsEnabled bool   `json:"is_enabled"`
}

func validPage(before string, limit int) bool {
	if limit < 1 || limit > 100 {
		return false
	}
	if before != "" {
		_, err := ulid.ParseStrict(before)
		return err == nil
	}
	return true
}

// ListUsers 复验当前管理员后分页读取账户元数据；外键关联在同一查询中完成，避免逐用户查询。
func (service *Service) ListUsers(ctx context.Context, actor identity.Principal, before string, limit int) ([]UserSummary, error) {
	return listCatalog(ctx, service, actor, before, limit,
		`SELECT u.id,u.username,u.display_name,u.role,u.status,c.status,o.id,u.created_at FROM tombot.users u
		LEFT JOIN tombot.computers c ON c.user_id=u.id LEFT JOIN tombot.user_creation_operations o ON o.user_id=u.id
		WHERE ($1='' OR u.id<$1) ORDER BY u.id DESC LIMIT $2`,
		func(rows *sql.Rows) (UserSummary, error) {
			var user UserSummary
			err := rows.Scan(&user.ID, &user.Username, &user.DisplayName, &user.Role, &user.Status, &user.ComputerStatus, &user.CreationOperationID, &user.CreatedAt)
			return user, err
		})
}

// ListNodes 读取节点状态和已预留容量；离线节点仍可见，由界面说明不可选择的原因。
func (service *Service) ListNodes(ctx context.Context, actor identity.Principal, before string, limit int) ([]NodeSummary, error) {
	return listCatalog(ctx, service, actor, before, limit,
		`SELECT id,name,tombot.computer_node_status(status,runtime_id,control_expires_at,capability_expires_at),capacity,reserved FROM tombot.computer_nodes WHERE ($1='' OR id<$1) ORDER BY id DESC LIMIT $2`,
		func(rows *sql.Rows) (NodeSummary, error) {
			var node NodeSummary
			err := rows.Scan(&node.ID, &node.Name, &node.Status, &node.Capacity, &node.Reserved)
			return node, err
		})
}

// ListImages 读取不可变镜像引用，停用镜像不能因此绕过开户时的启用校验。
func (service *Service) ListImages(ctx context.Context, actor identity.Principal, before string, limit int) ([]ImageSummary, error) {
	return listCatalog(ctx, service, actor, before, limit,
		`SELECT id,reference,is_enabled FROM tombot.computer_images WHERE ($1='' OR id<$1) ORDER BY id DESC LIMIT $2`,
		func(rows *sql.Rows) (ImageSummary, error) {
			var image ImageSummary
			err := rows.Scan(&image.ID, &image.Reference, &image.IsEnabled)
			return image, err
		})
}

func listCatalog[T any](ctx context.Context, service *Service, actor identity.Principal, before string, limit int, query string, scan func(*sql.Rows) (T, error)) ([]T, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, false); err != nil {
		return nil, err
	}
	if !validPage(before, limit) {
		return nil, ErrInvalidInput
	}
	values := make([]T, 0, limit)
	err := service.transaction(ctx, func(transaction *sql.Tx) (result error) {
		rows, err := transaction.QueryContext(ctx, query, before, limit)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, rows.Close()) }()
		for rows.Next() {
			value, err := scan(rows)
			if err != nil {
				return err
			}
			values = append(values, value)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return values, nil
}
