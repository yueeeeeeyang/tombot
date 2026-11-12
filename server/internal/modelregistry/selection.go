package modelregistry

import (
	"context"
	"database/sql"
	"errors"

	"io.yueyang/tom/internal/identity"
)

// SetDefault 串行替换平台默认策略；仅接受已发布且当前可用的版本。
func (service *Service) SetDefault(ctx context.Context, actor identity.Principal, requestID, policyID string, expectedRevision int64) error {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return err
	}
	if !validID(policyID) || expectedRevision < 1 {
		return ErrInvalidInput
	}
	return service.transaction(ctx, func(transaction *sql.Tx) error {
		if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('tombot.model.default',0))`); err != nil {
			return err
		}
		if err := lockPolicy(ctx, transaction, policyID); err != nil {
			return err
		}
		var revision int64
		var number *int64
		if err := transaction.QueryRowContext(ctx, `SELECT revision,current_version FROM tombot.model_policies WHERE id=$1`, policyID).Scan(&revision, &number); err != nil {
			return err
		}
		if revision != expectedRevision {
			return ErrConflict
		}
		if number == nil {
			return ErrUnavailable
		}
		state, err := readVersion(ctx, transaction, policyID, *number, true)
		if err != nil {
			return err
		}
		if !state.isAvailable || state.version.Status != "published" {
			return ErrUnavailable
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.model_policies SET is_default=false,revision=revision+1 WHERE is_default AND id<>$1`, policyID); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.model_policies SET is_default=true,revision=revision+1 WHERE id=$1`, policyID); err != nil {
			return err
		}
		return audit(ctx, transaction, actor, requestID, "设置平台默认模型策略", policyID, struct{}{})
	})
}

// Assign 为用户或其机器人分配策略；空策略表示继承上一级，期望值用于防止丢失更新。
func (service *Service) Assign(ctx context.Context, actor identity.Principal, requestID, userID, botID, policyID, expectedPolicyID string) error {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return err
	}
	if !validID(userID) || (botID != "" && !validID(botID)) || (policyID != "" && !validID(policyID)) || (expectedPolicyID != "" && !validID(expectedPolicyID)) {
		return ErrInvalidInput
	}
	if botID != "" {
		if _, err := service.tenantPolicy(ctx, userID, botID, false); err != nil {
			return err
		}
	}
	return service.transaction(ctx, func(transaction *sql.Tx) error {
		if err := requireRow(transaction.QueryRowContext(ctx, `SELECT id FROM tombot.users WHERE id=$1 AND status NOT IN ('deleting','deleted') FOR UPDATE`, userID)); err != nil {
			return err
		}
		table := "user_model_assignments"
		where := `user_id=$1`
		arguments := []any{userID}
		if botID != "" {
			table = "bot_model_assignments"
			where += ` AND bot_id=$2`
			arguments = append(arguments, botID)
		}
		var current string
		err := transaction.QueryRowContext(ctx, `SELECT policy_id FROM tombot.`+table+` WHERE `+where, arguments...).Scan(&current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if current != expectedPolicyID {
			return ErrConflict
		}
		if policyID == "" {
			if _, err = transaction.ExecContext(ctx, `DELETE FROM tombot.`+table+` WHERE `+where, arguments...); err != nil {
				return err
			}
		} else {
			var number *int64
			err = transaction.QueryRowContext(ctx, `SELECT current_version FROM tombot.model_policies WHERE id=$1`, policyID).Scan(&number)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if number == nil {
				return ErrUnavailable
			}
			state, err := readVersion(ctx, transaction, policyID, *number, true)
			if err != nil {
				return err
			}
			if !state.isAvailable || state.version.Status != "published" {
				return ErrUnavailable
			}
			if botID == "" {
				_, err = transaction.ExecContext(ctx, `INSERT INTO tombot.user_model_assignments(user_id,policy_id) VALUES($1,$2) ON CONFLICT(user_id) DO UPDATE SET policy_id=EXCLUDED.policy_id,updated_at=transaction_timestamp()`, userID, policyID)
			} else {
				_, err = transaction.ExecContext(ctx, `INSERT INTO tombot.bot_model_assignments(user_id,bot_id,policy_id) VALUES($1,$2,$3) ON CONFLICT(user_id,bot_id) DO UPDATE SET policy_id=EXCLUDED.policy_id,updated_at=transaction_timestamp()`, userID, botID, policyID)
			}
			if err != nil {
				return err
			}
		}
		return audit(ctx, transaction, actor, requestID, "分配用户模型策略", userID, map[string]any{"bot_id": botID, "policy_id": policyID})
	})
}

func (service *Service) tenantPolicy(ctx context.Context, userID, botID string, requireActive bool) (string, error) {
	var policyID string
	err := service.tenant.Snapshot(ctx, userID, func(transaction *sql.Tx) error {
		err := transaction.QueryRowContext(ctx, `SELECT coalesce(bm.policy_id,um.policy_id,'') FROM tombot.bots b JOIN tombot.users u ON u.id=b.user_id
			LEFT JOIN tombot.bot_model_assignments bm ON bm.user_id=b.user_id AND bm.bot_id=b.id
			LEFT JOIN tombot.user_model_assignments um ON um.user_id=b.user_id
			WHERE b.user_id=$1 AND b.id=$2 AND NOT b.is_archived AND (NOT $3::boolean OR u.status='active')`, userID, botID, requireActive).Scan(&policyID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return "", errors.New("读取机器人模型归属失败")
	}
	return policyID, nil
}

// Resolve 供受信调度器在新运行开始时固定版本；用户与机器人必须来自已授权的任务记录。
// 优先级为机器人特例、用户策略、平台默认，选中项不可用时明确失败，不查找其他可用项。
func (service *Service) Resolve(ctx context.Context, userID, botID string) (Selection, error) {
	if !validID(userID) || !validID(botID) {
		return Selection{}, ErrInvalidInput
	}
	policyID, err := service.tenantPolicy(ctx, userID, botID, true)
	if err != nil {
		return Selection{}, err
	}
	selection := Selection{UserID: userID, BotID: botID}
	err = service.transaction(ctx, func(transaction *sql.Tx) error {
		var number *int64
		query := `SELECT id,current_version FROM tombot.model_policies WHERE id=$1`
		if policyID == "" {
			query = `SELECT id,current_version FROM tombot.model_policies WHERE is_default AND $1=''`
		}
		err := transaction.QueryRowContext(ctx, query, policyID).Scan(&selection.PolicyID, &number)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnavailable
		}
		if err != nil {
			return err
		}
		if number == nil {
			return ErrUnavailable
		}
		state, err := readVersion(ctx, transaction, selection.PolicyID, *number, false)
		if err != nil {
			return err
		}
		if !state.isAvailable || state.version.Status != "published" {
			return ErrUnavailable
		}
		selection.Version = *number
		return nil
	})
	if err != nil {
		return Selection{}, err
	}
	return selection, nil
}

// Use 仅供受信执行器使用运行记录中固定的选择；每次调用重新检查用户、配置和凭据撤销。
// 回调在事务外执行，返回后凭据清零；不得把回调或 Selection 暴露为客户端任意模型代理。
func (service *Service) Use(ctx context.Context, selection Selection, execute func(context.Context, Configuration, []byte) error) error {
	if !validID(selection.UserID) || !validID(selection.BotID) || !validID(selection.PolicyID) || selection.Version < 1 || execute == nil {
		return ErrInvalidInput
	}
	if _, err := service.tenantPolicy(ctx, selection.UserID, selection.BotID, true); err != nil {
		return err
	}
	var configuration Configuration
	var plaintext []byte
	defer func() { clear(plaintext) }()
	if err := service.transaction(ctx, func(transaction *sql.Tx) error {
		state, err := readVersion(ctx, transaction, selection.PolicyID, selection.Version, true)
		if err != nil {
			return err
		}
		if !state.isAvailable || state.version.Status != "published" {
			return ErrUnavailable
		}
		configuration = state.version.Configuration
		plaintext, err = service.decrypt(ctx, transaction, state.version)
		return err
	}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// 数据库校验是本次调用的接纳点；已接纳的外部请求不能靠事后撤销保证回滚。
	return execute(ctx, configuration, plaintext)
}
