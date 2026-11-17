package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/groupchat"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/provider"
)

type groupPostTool struct{ executor *Service }

func (groupPostTool) Schema() provider.ToolSchema { return groupchat.Schema() }
func (groupPostTool) UsesComputer() bool          { return false }
func (groupPostTool) RequiresVision() bool        { return false }

// Execute 将公开发言与工具回执同事务提交，恢复只重放回执，不重复发布或唤醒成员。
func (tool groupPostTool) Execute(ctx context.Context, invocation Invocation) (botagent.ToolResult, error) {
	var result botagent.ToolResult
	err := tool.executor.worker(ctx, invocation.Lease, false, func(tx *sql.Tx) error {
		lease := invocation.Lease
		if err := available(ctx, tx, lease); err != nil {
			return err
		}
		var input struct {
			Message  string   `json:"message"`
			Mentions []string `json:"mentions"`
			Reply    string   `json:"reply_to_id"`
		}
		if decodeToolInput(invocation.Call.Arguments, &input, groupchat.Schema()) != nil || !validText(input.Message, 16<<10) {
			return ErrInvalidInput
		}
		operation, err := scanOperation(tx.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2 AND id=$3`, lease.UserID, lease.RunID, invocation.OperationID))
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(invocation.Call)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(encoded)
		if invocation.Call.Name != "post_to_group" || operation.Kind != "tool" || operation.Status != "started" || operation.FencingToken != lease.FencingToken || operation.Attempt != lease.Attempt || !bytes.Equal(digest[:], operation.RequestDigest) {
			return ErrConflict
		}
		var locked string
		if err = tx.QueryRowContext(ctx, `SELECT c.id FROM tombot.conversations c JOIN tombot.group_deliveries d ON d.user_id=c.user_id AND d.conversation_id=c.id WHERE d.user_id=$1 AND d.run_id=$2 FOR UPDATE OF c`, lease.UserID, lease.RunID).Scan(&locked); err != nil {
			return err
		}
		if err = available(ctx, tx, lease); err != nil {
			return err
		}
		members, err := groupMembers(ctx, tx, lease.UserID, lease.ConversationID)
		if err != nil {
			return err
		}
		replyBot, err := groupReplyBot(ctx, tx, lease.UserID, lease.ConversationID, input.Reply)
		if err != nil {
			return err
		}
		input.Reply, err = groupReplyReference(ctx, tx, lease, input.Reply)
		if err != nil {
			return err
		}
		if replyBot != "" {
			input.Mentions = append(input.Mentions, replyBot)
		}
		mentions, err := groupchat.Mentions(input.Message, members, input.Mentions)
		if err != nil {
			return err
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tombot.messages WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID).Scan(&count); err != nil {
			return err
		}
		if count >= groupchat.MaxPosts-1 {
			return ErrCapacity
		}
		message := ids.New()
		if err = writeGroupMessage(ctx, tx, lease.UserID, lease.ConversationID, lease.TurnID, lease.RunID, lease.BotID, message, input.Reply, []Block{{Kind: "text", Text: input.Message, Mentions: mentions}}, "complete"); err != nil {
			return err
		}
		encoded, err = json.Marshal(map[string]string{"message_id": message, "status": "posted"})
		if err != nil {
			return err
		}
		result = botagent.ToolResult{Content: string(encoded)}
		encoded, err = json.Marshal(result)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE tombot.run_operations SET status='succeeded',result=$4,finished_at=clock_timestamp() WHERE user_id=$1 AND run_id=$2 AND id=$3`, lease.UserID, lease.RunID, operation.ID, string(encoded))
		return err
	})
	if errors.Is(err, ErrInvalidInput) || errors.Is(err, groupchat.ErrMention) || errors.Is(err, ErrCapacity) {
		return botagent.ToolResult{IsError: true, Content: "GROUP_POST_REJECTED: Use valid text, current group member IDs (or 'user') and a message from this group. At most 15 tool posts per run. Nothing was posted."}, nil
	}
	return result, err
}

func isGroupRun(ctx context.Context, tx *sql.Tx, lease Lease) (bool, error) {
	var group bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.group_deliveries WHERE user_id=$1 AND run_id=$2)`, lease.UserID, lease.RunID).Scan(&group)
	return group, err
}

// groupReplyReference 统一工具发言与最终答复的引用规则，仅保留明确提及当前猫咪的公开消息。
// 工具入口已校验引用归属；隐藏私信触发消息在收尾时只返回空引用。
func groupReplyReference(ctx context.Context, tx *sql.Tx, lease Lease, message string) (string, error) {
	if message == "" {
		return "", nil
	}
	var mentioned bool
	err := tx.QueryRowContext(ctx, `SELECT visible_to_bot_id IS NULL AND blocks @> jsonb_build_array(jsonb_build_object('mentions',jsonb_build_array(jsonb_build_object('id',$4::text)))) FROM tombot.messages WHERE user_id=$1 AND conversation_id=$2 AND id=$3`, lease.UserID, lease.ConversationID, message, lease.BotID).Scan(&mentioned)
	if err != nil {
		return "", err
	}
	if !mentioned {
		return "", nil
	}
	return message, nil
}

// finishGroupAnswer 只在触发消息明确提及当前猫咪时绑定引用；显式 @ 才继续转交，普通答复不会唤醒全群。
func finishGroupAnswer(ctx context.Context, tx *sql.Tx, lease Lease, outcome Outcome) error {
	var trigger string
	var roster []byte
	if err := tx.QueryRowContext(ctx, `SELECT trigger_message_id,roster FROM tombot.group_deliveries WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID).Scan(&trigger, &roster); err != nil {
		return err
	}
	var members []groupchat.Member
	if err := json.Unmarshal(roster, &members); err != nil {
		return err
	}
	trigger, err := groupReplyReference(ctx, tx, lease, trigger)
	if err != nil {
		return err
	}
	mentions, err := groupchat.Mentions(outcome.Answer, members, nil)
	if errors.Is(err, groupchat.ErrMention) {
		if noticeErr := writeGroupMessage(ctx, tx, lease.UserID, lease.ConversationID, lease.TurnID, lease.RunID, "", ids.New(), "", []Block{{Kind: "text", Text: "消息中的成员名称不唯一，未自动转交。请从 @ 菜单选择具体成员。"}}, "complete"); noticeErr != nil {
			return noticeErr
		}
	} else if err != nil {
		return err
	}
	// 工具已发表完全相同内容时，保留已有消息，不再生成重复气泡。
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.messages WHERE user_id=$1 AND run_id=$2 AND blocks->0->>'text'=$3)`, lease.UserID, lease.RunID, outcome.Answer).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	status := "complete"
	if outcome.Status != "succeeded" {
		status = "interrupted"
	}
	return writeGroupMessage(ctx, tx, lease.UserID, lease.ConversationID, lease.TurnID, lease.RunID, lease.BotID, ids.New(), trigger, []Block{{Kind: "text", Text: outcome.Answer, Mentions: mentions}}, status)
}

// followGroupMentions 只在成员收尾时唤醒已公开提及的对象；限额中止有可见通知，不悄悄丢弃。
func followGroupMentions(ctx context.Context, tx *sql.Tx, lease Lease) error {
	members, err := groupMembers(ctx, tx, lease.UserID, lease.ConversationID)
	if errors.Is(err, ErrUnavailable) {
		return nil
	}
	if err != nil {
		return err
	}
	var depth, runs int
	if err = tx.QueryRowContext(ctx, `SELECT depth,(SELECT count(*) FROM tombot.runs WHERE user_id=$1 AND turn_id=$3) FROM tombot.group_deliveries WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID, lease.TurnID).Scan(&depth, &runs); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,blocks FROM tombot.messages WHERE user_id=$1 AND run_id=$2 AND status='complete' ORDER BY sequence LIMIT $3`, lease.UserID, lease.RunID, groupchat.MaxPosts)
	if err != nil {
		return err
	}
	type post struct {
		id       string
		mentions []groupchat.Mention
	}
	posts := []post{}
	for rows.Next() {
		var id string
		var encoded []byte
		if err = rows.Scan(&id, &encoded); err != nil {
			return errors.Join(err, rows.Close())
		}
		var blocks []Block
		if err = json.Unmarshal(encoded, &blocks); err != nil {
			return errors.Join(err, rows.Close())
		}
		mentions := []groupchat.Mention{}
		for _, block := range blocks {
			mentions = append(mentions, block.Mentions...)
		}
		posts = append(posts, post{id, mentions})
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, post := range posts {
		recipients := groupchat.Recipients(members, post.mentions, "", lease.BotID)
		if len(recipients) == 0 {
			continue
		}
		stopped := depth >= groupchat.MaxDepth || runs+len(recipients) > groupchat.MaxRuns
		if !stopped {
			err = groupCapacity(ctx, tx, lease.UserID, len(recipients))
			if err != nil && !errors.Is(err, ErrCapacity) {
				return err
			}
			stopped = errors.Is(err, ErrCapacity)
		}
		if stopped {
			return writeGroupMessage(ctx, tx, lease.UserID, lease.ConversationID, lease.TurnID, lease.RunID, "", ids.New(), "", []Block{{Kind: "text", Text: "本轮群聊已达到转交轮次或队列上限，后续提及未继续执行。你可以再发一条消息继续。"}}, "complete")
		}
		queued, err := enqueueGroup(ctx, tx, lease.UserID, lease.ConversationID, lease.TurnID, post.id, depth+1, members, recipients)
		if err != nil {
			return err
		}
		runs += len(queued)
	}
	return nil
}
