package modelusage

import (
	"context"
	"database/sql"
	"errors"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
)

type Service struct{ tenant, dispatch *postgres.Database }

// New 将计量写入与全局发现分离，调度连接没有正文或供应商计数读取权。
func New(tenant, dispatch *postgres.Database) (*Service, error) {
	if tenant == nil || dispatch == nil || tenant.Role() != postgres.TenantRole || dispatch.Role() != postgres.DispatchRole {
		return nil, ErrInvalidInput
	}
	return &Service{tenant, dispatch}, nil
}

// Reconcile 有界处理迟到证据和超过调用硬时限的预留，不以运行租约到期代替调用结束。
// 超时仅释放本地调用名额，计量未知时继续保留 token 额度。
func (service *Service) Reconcile(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, ErrInvalidInput
	}
	type target struct{ userID, id string }
	var targets []target
	err := service.dispatch.Platform(ctx, func(transaction *sql.Tx) (result error) {
		rows, err := transaction.QueryContext(ctx, `SELECT user_id,id FROM tombot.usage_reservations WHERE needs_reconcile OR state='active' AND slot_expires_at<=clock_timestamp() ORDER BY slot_expires_at,user_id,id LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, rows.Close()) }()
		for rows.Next() {
			var item target
			if err = rows.Scan(&item.userID, &item.id); err != nil {
				return err
			}
			targets = append(targets, item)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, item := range targets {
		changed := false
		err = service.tenant.Tenant(ctx, item.userID, func(transaction *sql.Tx) error {
			if err := lockAccount(ctx, transaction, item.userID); err != nil {
				return err
			}
			stored, err := readReservation(ctx, transaction, item.userID, item.id)
			if err != nil {
				return err
			}
			var pending bool
			if err = transaction.QueryRowContext(ctx, `SELECT needs_reconcile FROM tombot.usage_reservations WHERE user_id=$1 AND id=$2`, item.userID, item.id).Scan(&pending); err != nil {
				return err
			}
			if !pending {
				if stored.State != "active" || !stored.Expired {
					return nil
				}
				if err = appendEvidence(ctx, transaction, stored, ids.New(), "timeout", Observation{}); err != nil {
					return err
				}
			}
			if err = applyEvidence(ctx, transaction, stored); err != nil {
				return err
			}
			changed = true
			return nil
		})
		if err != nil {
			return count, err
		}
		if changed {
			count++
		}
	}
	return count, nil
}
