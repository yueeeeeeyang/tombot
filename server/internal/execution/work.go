package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"io.yueyang/tom/internal/modelregistry"
)

func available(ctx context.Context, transaction *sql.Tx, lease Lease) error {
	var valid bool
	err := transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.users u JOIN tombot.bots b ON b.user_id=u.id JOIN tombot.conversations c ON c.user_id=u.id
		WHERE u.id=$1 AND b.id=$2 AND c.id=$3 AND u.status='active' AND NOT b.is_archived AND NOT c.is_archived)`, lease.UserID, lease.BotID, lease.ConversationID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrUnavailable
	}
	return nil
}

// Start 激活已领取运行，返回固定机器人设定、有界历史和已知操作结果；不调用外部模型。
func (service *Service) Start(ctx context.Context, lease Lease) (Work, error) {
	var work Work
	err := service.worker(ctx, lease, false, func(transaction *sql.Tx) error {
		if err := available(ctx, transaction, lease); err != nil {
			return err
		}
		run, err := readRun(ctx, transaction, lease.UserID, lease.RunID)
		if err != nil {
			return err
		}
		if run.Status != "queued" && run.Status != "running" {
			return ErrConflict
		}
		if run.Status == "queued" {
			if _, err = transaction.ExecContext(ctx, `UPDATE tombot.runs SET status='running',started_at=COALESCE(started_at,clock_timestamp()) WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID); err != nil {
				return err
			}
			if _, err = transaction.ExecContext(ctx, `UPDATE tombot.turns SET status='running' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.TurnID); err != nil {
				return err
			}
			if err = event(ctx, transaction, lease.UserID, "run.started", lease.RunID, map[string]int{"attempt": lease.Attempt}); err != nil {
				return err
			}
		}
		work.Run, err = readRun(ctx, transaction, lease.UserID, lease.RunID)
		if err != nil {
			return err
		}
		var snapshot []byte
		var policy *string
		var version *int64
		if err = transaction.QueryRowContext(ctx, `SELECT bot_snapshot,model_policy_id,model_version FROM tombot.runs WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID).Scan(&snapshot, &policy, &version); err != nil {
			return err
		}
		if err = json.Unmarshal(snapshot, &work.Bot); err != nil {
			return err
		}
		if policy != nil && version != nil {
			work.Selection = &modelregistry.Selection{UserID: lease.UserID, BotID: lease.BotID, PolicyID: *policy, Version: *version}
		}
		messages, err := listMessages(ctx, transaction, lease.UserID, lease.ConversationID, 0, service.options.ContextMessages+1)
		if err != nil {
			return err
		}
		used := 0
		work.Messages = make([]Message, 0, min(len(messages), service.options.ContextMessages))
		for index, message := range messages {
			if index >= service.options.ContextMessages {
				work.HistoryTruncated = true
				break
			}
			encoded, err := json.Marshal(message.Blocks)
			if err != nil {
				return err
			}
			if used+len(encoded) > service.options.ContextBytes {
				if len(work.Messages) == 0 {
					return ErrContextLimit
				}
				work.HistoryTruncated = true
				break
			}
			used += len(encoded)
			if message.Status == "complete" {
				work.Messages = append(work.Messages, message)
			}
		}
		slices.Reverse(work.Messages)
		work.Operations, err = readOperations(ctx, transaction, lease.UserID, lease.RunID, service.options.MaxOperations)
		return err
	})
	if err != nil {
		return Work{}, err
	}
	return work, nil
}

// BindModel 在首次调用前持久固定策略版本；恢复运行永远返回原绑定，不重新路由。
func (service *Service) BindModel(ctx context.Context, lease Lease) (modelregistry.Selection, error) {
	var selection *modelregistry.Selection
	read := func(transaction *sql.Tx) error {
		var policy *string
		var version *int64
		if err := transaction.QueryRowContext(ctx, `SELECT model_policy_id,model_version FROM tombot.runs WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID).Scan(&policy, &version); err != nil {
			return err
		}
		if policy != nil && version != nil {
			selection = &modelregistry.Selection{UserID: lease.UserID, BotID: lease.BotID, PolicyID: *policy, Version: *version}
		}
		return nil
	}
	if err := service.worker(ctx, lease, false, read); err != nil {
		return modelregistry.Selection{}, err
	}
	if selection != nil {
		return *selection, nil
	}
	resolved, err := service.resolver.Resolve(ctx, lease.UserID, lease.BotID)
	if err != nil {
		return modelregistry.Selection{}, err
	}
	if resolved.UserID != lease.UserID || resolved.BotID != lease.BotID || !validID(resolved.PolicyID) || resolved.Version < 1 {
		return modelregistry.Selection{}, ErrInvalidInput
	}
	err = service.worker(ctx, lease, false, func(transaction *sql.Tx) error {
		if err := available(ctx, transaction, lease); err != nil {
			return err
		}
		if err := read(transaction); err != nil {
			return err
		}
		if selection != nil {
			return nil
		}
		var state string
		if err := transaction.QueryRowContext(ctx, `SELECT status FROM tombot.runs WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID).Scan(&state); err != nil {
			return err
		}
		if state != "running" {
			return ErrConflict
		}
		_, err := transaction.ExecContext(ctx, `UPDATE tombot.runs SET model_policy_id=$3,model_version=$4 WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID, resolved.PolicyID, resolved.Version)
		if err != nil {
			return err
		}
		selection = &resolved
		return nil
	})
	if err != nil {
		return modelregistry.Selection{}, err
	}
	if selection == nil {
		return modelregistry.Selection{}, errors.New("运行缺少模型绑定")
	}
	return *selection, nil
}
