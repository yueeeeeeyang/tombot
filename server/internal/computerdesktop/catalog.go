// Package computerdesktop 管理用户电脑内稳定的机器人桌面身份，不启动桌面或授予执行权。
package computerdesktop

import (
	"context"
	"database/sql"
	"errors"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

var (
	ErrInvalidInput = errors.New("桌面登记参数无效")
	ErrCapacity     = errors.New("用户电脑桌面身份数量已达到上限")
)

func validIDs(ids ...string) bool {
	for _, id := range ids {
		if _, err := ulid.ParseStrict(id); err != nil {
			return false
		}
	}
	return true
}

// Lookup 在调用方已经鉴权的租户事务内读取桌面；未登记或不属于当前用户时返回 sql.ErrNoRows。
// 仅返回逻辑身份，实际节点操作还需验证电脑实例、桌面租约和控制代次。
func Lookup(ctx context.Context, transaction *sql.Tx, userID, desktopID string) (computer.DesktopIdentity, error) {
	if transaction == nil || !validIDs(userID, desktopID) {
		return computer.DesktopIdentity{}, ErrInvalidInput
	}
	var identity computer.DesktopIdentity
	err := transaction.QueryRowContext(ctx, `SELECT user_id,computer_id,bot_id,id,linux_uid,shared_gid,display_number FROM tombot.bot_desktops WHERE user_id=$1 AND id=$2`, userID, desktopID).
		Scan(&identity.UserID, &identity.ComputerID, &identity.BotID, &identity.DesktopID, &identity.UID, &identity.GID, &identity.DisplayNumber)
	if err != nil {
		return computer.DesktopIdentity{}, err
	}
	if err = identity.Validate(); err != nil {
		return computer.DesktopIdentity{}, err
	}
	return identity, nil
}

// Register 与机器人创建共用事务；调用前须插入同一用户、机器人和桌面 ID 的机器人记录。
// 只在短事务内锁定编号分配行；相同身份重试不会消耗编号，失败由调用方回滚整个事务。
func Register(ctx context.Context, transaction *sql.Tx, userID, botID, desktopID string) (computer.DesktopIdentity, error) {
	return register(ctx, transaction, userID, botID, desktopID, "chat")
}

// TaskDesktop 为猫咪惰性分配一个独立后台桌面；身份永久保留，重启和并发申请不会重复分配。
func TaskDesktop(ctx context.Context, tx *sql.Tx, userID, botID string) (computer.DesktopIdentity, error) {
	if tx == nil || !validIDs(userID, botID) {
		return computer.DesktopIdentity{}, ErrInvalidInput
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM tombot.bots WHERE user_id=$1 AND id=$2 AND NOT is_archived FOR UPDATE`, userID, botID).Scan(&id)
	if err != nil {
		return computer.DesktopIdentity{}, err
	}
	err = tx.QueryRowContext(ctx, `SELECT id FROM tombot.bot_desktops WHERE user_id=$1 AND bot_id=$2 AND purpose='task'`, userID, botID).Scan(&id)
	if err == nil {
		return Lookup(ctx, tx, userID, id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return computer.DesktopIdentity{}, err
	}
	return register(ctx, tx, userID, botID, ids.New(), "task")
}
func register(ctx context.Context, transaction *sql.Tx, userID, botID, desktopID, purpose string) (computer.DesktopIdentity, error) {
	if transaction == nil || !validIDs(userID, botID, desktopID) {
		return computer.DesktopIdentity{}, ErrInvalidInput
	}
	var computerID string
	err := transaction.QueryRowContext(ctx, `SELECT c.id FROM tombot.computers c JOIN tombot.bots b ON b.user_id=c.user_id
		WHERE c.user_id=$1 AND b.id=$2 AND ($4='task' OR b.desktop_id=$3) AND c.status<>'deleted'`, userID, botID, desktopID, purpose).Scan(&computerID)
	if err != nil {
		return computer.DesktopIdentity{}, err
	}
	// 首次并发登记可能命中主键或复合唯一索引，随后仍锁定并核对完整电脑归属。
	if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.desktop_identity_heads(user_id,computer_id,shared_gid) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, userID, computerID, computer.DesktopSharedGID); err != nil {
		return computer.DesktopIdentity{}, err
	}
	var identity computer.DesktopIdentity
	identity.UserID, identity.BotID, identity.DesktopID = userID, botID, desktopID
	err = transaction.QueryRowContext(ctx, `SELECT computer_id,shared_gid,last_display_number FROM tombot.desktop_identity_heads WHERE user_id=$1 FOR UPDATE`, userID).
		Scan(&identity.ComputerID, &identity.GID, &identity.DisplayNumber)
	if err != nil {
		return computer.DesktopIdentity{}, err
	}
	if identity.ComputerID != computerID {
		return computer.DesktopIdentity{}, ErrInvalidInput
	}
	previous, err := Lookup(ctx, transaction, userID, desktopID)
	if err == nil {
		if previous.BotID != botID || previous.ComputerID != computerID {
			return computer.DesktopIdentity{}, ErrInvalidInput
		}
		return previous, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return computer.DesktopIdentity{}, err
	}
	if identity.DisplayNumber >= computer.MaxDesktops {
		return computer.DesktopIdentity{}, ErrCapacity
	}
	identity.DisplayNumber++
	identity.UID = computer.DesktopUIDBase + identity.DisplayNumber
	if err = identity.Validate(); err != nil {
		return computer.DesktopIdentity{}, err
	}
	if _, err = transaction.ExecContext(ctx, `UPDATE tombot.desktop_identity_heads SET last_display_number=$2 WHERE user_id=$1`, userID, identity.DisplayNumber); err != nil {
		return computer.DesktopIdentity{}, err
	}
	_, err = transaction.ExecContext(ctx, `INSERT INTO tombot.bot_desktops(user_id,id,bot_id,computer_id,linux_uid,shared_gid,display_number,purpose) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		userID, desktopID, botID, computerID, identity.UID, identity.GID, identity.DisplayNumber, purpose)
	if err != nil {
		return computer.DesktopIdentity{}, err
	}
	return identity, nil
}
