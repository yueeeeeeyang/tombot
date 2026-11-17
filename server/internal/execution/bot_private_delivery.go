package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"io.yueyang/tom/internal/botmessaging"
	"io.yueyang/tom/internal/groupchat"
)

// stagePrivateReply 在接收方收尾事务中保存回复；仍有子私信时等待其恢复结果，不提前宣告完成。
func stagePrivateReply(ctx context.Context, tx *sql.Tx, lease Lease, outcome Outcome) error {
	current, err := readPrivateContext(ctx, tx, lease)
	if err != nil {
		return err
	}
	if current.parent == "" {
		return nil
	}
	var status string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM tombot.bot_private_messages WHERE user_id=$1 AND id=$2 FOR UPDATE`, lease.UserID, current.parent).Scan(&status); err != nil {
		return err
	}
	if status != "pending" && status != "waiting" {
		return nil
	}
	var pending bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.bot_private_messages WHERE user_id=$1 AND parent_id=$2 AND status IN ('pending','waiting','ready'))`, lease.UserID, current.parent).Scan(&pending); err != nil {
		return err
	}
	status = "ready"
	if pending && outcome.Status == "succeeded" {
		status = "waiting"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tombot.bot_private_messages SET status=$3,response_text=$4,response_status=$5,error_code=$6 WHERE user_id=$1 AND id=$2`, lease.UserID, current.parent, status, outcome.Answer, outcome.Status, outcome.ErrorCode); err != nil {
		return err
	}
	if status == "waiting" {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tombot.bot_private_delivery(user_id,message_id,bot_id,conversation_id) SELECT user_id,id,sender_bot_id,source_conversation_id FROM tombot.bot_private_messages WHERE user_id=$1 AND id=$2`, lease.UserID, current.parent)
	return err
}

type privateDelivery struct{ user, message, bot, conversation string }

// DeliverPrivateReplies 有界重查持久回复，在发起猫咪当前运行结束后恢复原会话；不重跑接收方。
func (service *Service) DeliverPrivateReplies(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, ErrInvalidInput
	}
	pending := []privateDelivery{}
	err := service.dispatch.Platform(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT d.user_id,d.message_id,d.bot_id,d.conversation_id FROM tombot.bot_private_delivery d WHERE d.available_at<=clock_timestamp() AND NOT EXISTS(SELECT 1 FROM tombot.jobs j WHERE j.user_id=d.user_id AND j.bot_id=d.bot_id AND j.status<>'finished') ORDER BY d.available_at,d.user_id,d.message_id LIMIT $1`, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item privateDelivery
			if err = rows.Scan(&item.user, &item.message, &item.bot, &item.conversation); err != nil {
				return errors.Join(err, rows.Close())
			}
			pending = append(pending, item)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return 0, translate(err)
	}
	count := 0
	var failures error
	for _, item := range pending {
		delivered := false
		err = service.tenant.Tenant(ctx, item.user, func(tx *sql.Tx) error {
			if err := lockDispatch(ctx, tx, item.user); err != nil {
				return err
			}
			var kind string
			var unavailable bool
			if err := tx.QueryRowContext(ctx, `SELECT c.kind,c.is_archived OR b.is_archived OR u.status<>'active' FROM tombot.conversations c JOIN tombot.bots b ON b.user_id=c.user_id AND b.id=$3 JOIN tombot.users u ON u.id=c.user_id WHERE c.user_id=$1 AND c.id=$2 FOR UPDATE OF c`, item.user, item.conversation, item.bot).Scan(&kind, &unavailable); err != nil {
				return err
			}
			var recipient, name, answer, responseStatus, chain, parent string
			var canceled bool
			var path []byte
			err := tx.QueryRowContext(ctx, `SELECT p.recipient_bot_id,b.name,p.response_text,p.response_status,p.chain_id,COALESCE(p.parent_id,''),p.source_path,(root.status IN ('canceling','canceled') OR origin.status IN ('canceling','canceled')) FROM tombot.bot_private_delivery d JOIN tombot.bot_private_messages p ON p.user_id=d.user_id AND p.id=d.message_id JOIN tombot.bots b ON b.user_id=p.user_id AND b.id=p.recipient_bot_id JOIN tombot.runs root ON root.user_id=p.user_id AND root.id=p.chain_id JOIN tombot.runs origin ON origin.user_id=p.user_id AND origin.id=p.source_run_id WHERE d.user_id=$1 AND d.message_id=$2 AND p.status='ready' FOR UPDATE OF d,p`, item.user, item.message).Scan(&recipient, &name, &answer, &responseStatus, &chain, &parent, &path, &canceled)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if canceled {
				return discardPrivateReply(ctx, tx, item, "SOURCE_CANCELED", "发起运行已取消，私信回复保留记录但不恢复聊天")
			}
			if parent != "" {
				var active bool
				if err = tx.QueryRowContext(ctx, `SELECT status IN ('pending','waiting') FROM tombot.bot_private_messages WHERE user_id=$1 AND id=$2`, item.user, parent).Scan(&active); err != nil {
					return err
				}
				if !active {
					return discardPrivateReply(ctx, tx, item, "PARENT_FINISHED", "上级私信已终止，回复保留在私信记录中")
				}
			}
			var members []groupchat.Member
			if kind == "group" {
				members, err = groupMembers(ctx, tx, item.user, item.conversation)
				if errors.Is(err, ErrUnavailable) {
					unavailable = true
				} else if err != nil {
					return err
				}
				if !slices.ContainsFunc(members, func(member groupchat.Member) bool { return member.ID == item.bot }) {
					unavailable = true
				}
			}
			if unavailable {
				if parent != "" {
					return failPrivateParent(ctx, tx, item, parent)
				}
				return delayPrivateReply(ctx, tx, item)
			}
			var busy bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.jobs WHERE user_id=$1 AND (bot_id=$2 OR (conversation_id=$3 AND $4<>'group')) AND status<>'finished')`, item.user, item.bot, item.conversation, kind).Scan(&busy); err != nil {
				return err
			}
			if busy {
				return nil
			}
			if err = groupCapacity(ctx, tx, item.user, 1); errors.Is(err, ErrCapacity) {
				return delayPrivateReply(ctx, tx, item)
			} else if err != nil {
				return err
			}
			reference := botmessaging.Reference{MessageID: item.message, SenderID: recipient, SenderName: name, Kind: "reply", Status: responseStatus}
			if responseStatus != "succeeded" {
				answer = fmt.Sprintf("The peer %q did not complete this request (status: %s).\n%s", name, responseStatus, answer)
			}
			submitted, err := newPrivateInput(ctx, tx, item.user, item.conversation, item.bot, answer, reference, members)
			if err != nil {
				return err
			}
			current := privateContext{chain: chain, parent: parent}
			if err = json.Unmarshal(path, &current.path); err != nil {
				return err
			}
			if err = attachPrivateContext(ctx, tx, item.user, item.conversation, submitted, current); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE tombot.bot_private_messages SET status='delivered',delivery_run_id=$3 WHERE user_id=$1 AND id=$2`, item.user, item.message, submitted.RunID); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM tombot.bot_private_delivery WHERE user_id=$1 AND message_id=$2`, item.user, item.message); err != nil {
				return err
			}
			delivered = true
			return nil
		})
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("私信回复投递失败（用户 %s，私信 %s，猫咪 %s）：%w", item.user, item.message, item.bot, translate(err)))
		} else if delivered {
			count++
		}
	}
	return count, failures
}

func delayPrivateReply(ctx context.Context, tx *sql.Tx, item privateDelivery) error {
	_, err := tx.ExecContext(ctx, `UPDATE tombot.bot_private_delivery SET available_at=clock_timestamp()+interval '1 minute' WHERE user_id=$1 AND message_id=$2`, item.user, item.message)
	return err
}

// 中间猫咪不可用时沿原链返回失败，不能让仍可用的上级无限等待它恢复。
func failPrivateParent(ctx context.Context, tx *sql.Tx, item privateDelivery, parent string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE tombot.bot_private_messages SET status='ready',response_status='failed',response_text='',error_code='RESOURCE_UNAVAILABLE' WHERE user_id=$1 AND id=$2`, item.user, parent); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tombot.bot_private_delivery(user_id,message_id,bot_id,conversation_id) SELECT user_id,id,sender_bot_id,source_conversation_id FROM tombot.bot_private_messages WHERE user_id=$1 AND id=$2`, item.user, parent); err != nil {
		return err
	}
	return discardPrivateReply(ctx, tx, item, "PARENT_UNAVAILABLE", "中间猫咪不可用，已向上级返回失败并保留私信记录")
}

func discardPrivateReply(ctx context.Context, tx *sql.Tx, item privateDelivery, code, reason string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE tombot.bot_private_messages SET status='discarded',error_code=$3 WHERE user_id=$1 AND id=$2`, item.user, item.message, code); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tombot.bot_private_delivery WHERE user_id=$1 AND message_id=$2`, item.user, item.message); err != nil {
		return err
	}
	return event(ctx, tx, item.user, "private.reply.discarded", item.message, map[string]string{"bot_id": item.bot, "reason": reason})
}

// deliverPrivateReplies 与模型执行并行轮询，重启后仍从数据库继续投递，错误保留业务标识。
func (worker *Worker) deliverPrivateReplies(ctx context.Context) {
	for ctx.Err() == nil {
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := worker.executor.DeliverPrivateReplies(bounded, 100)
		cancel()
		if err != nil && ctx.Err() == nil {
			worker.logger.ErrorContext(ctx, "私信回复投递失败，等待下一轮核对", "reason", err)
		}
		if !waitPoll(ctx, time.Second) {
			return
		}
	}
}
