package computercontrol

import (
	"context"
	"database/sql"
	"errors"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/identity"
)

// ManageComputer 按登录会话解析唯一的电脑，客户端只能回传本次看到的版本。
func (service *Service) ManageComputer(ctx context.Context, p identity.Principal, version string) (computer.ComputerStatus, error) {
	var m computer.Maintenance
	err := service.tenant.Snapshot(ctx, p.UserID, func(tx *sql.Tx) error {
		if err := identity.CheckTenantSession(ctx, tx, p); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT c.user_id,c.id,c.node_id,c.container_id,c.generation FROM tombot.computers c WHERE c.user_id=$1 AND c.status='ready' AND NOT EXISTS(SELECT 1 FROM tombot.computer_image_upgrades u WHERE u.user_id=c.user_id AND u.computer_id=c.id AND u.state IN ('draining','switching','quarantined'))`, p.UserID).Scan(&m.UserID, &m.ComputerID, &m.NodeID, &m.ContainerID, &m.Generation)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return computer.ComputerStatus{}, ErrUnavailable
	}
	if err != nil {
		return computer.ComputerStatus{}, err
	}
	m.ExpectedVersion = version
	if m.Validate() != nil {
		return computer.ComputerStatus{}, ErrInvalidInput
	}
	manager, ok := service.node.(computer.ComputerManager)
	if !ok {
		return computer.ComputerStatus{}, ErrNode
	}
	bounded, stop := context.WithTimeout(ctx, service.options.NodeTimeout)
	defer stop()
	receipt, err := manager.ManageComputer(bounded, m)
	if err != nil || !receipt.Matches(m) {
		return computer.ComputerStatus{}, ErrNode
	}
	return receipt.ComputerStatus, nil
}
