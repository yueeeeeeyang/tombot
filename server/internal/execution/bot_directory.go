package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"io.yueyang/tom/internal/botmessaging"
	"io.yueyang/tom/internal/bots"
)

// 空闲同时排除已受理消息、后台任务和等待私信回复，防止把无租约误判为空闲。
const botStatusSQL = `CASE
 WHEN b.is_archived THEN 'archived'
 WHEN NOT (b.messaging_policy->>'accept_dm')::boolean THEN 'unavailable'
 WHEN EXISTS(SELECT 1 FROM tombot.jobs j WHERE j.user_id=b.user_id AND j.bot_id=b.id AND j.status='leased') THEN 'busy'
 WHEN EXISTS(SELECT 1 FROM tombot.jobs j WHERE j.user_id=b.user_id AND j.bot_id=b.id AND j.status='queued') THEN 'queued'
 WHEN EXISTS(SELECT 1 FROM tombot.bot_private_messages p WHERE p.user_id=b.user_id AND ((p.sender_bot_id=b.id AND p.status IN ('pending','waiting','ready')) OR (p.recipient_bot_id=b.id AND p.status='waiting'))) THEN 'waiting'
 ELSE 'idle' END`

func validBotQuery(query botmessaging.Query) bool {
	if query.After != "" && !validID(query.After) || query.BotID != "" && !validID(query.BotID) || query.After != "" && query.BotID != "" || len(query.RequiredTools) > 128 {
		return false
	}
	for _, name := range query.RequiredTools {
		if !bots.ValidToolName(name) {
			return false
		}
	}
	return true
}

// listBotDirectory 单条有界查询返回当前租户目录；分页与能力筛选不会加载私人提示词或记忆。
func listBotDirectory(ctx context.Context, tx *sql.Tx, user, self string, query botmessaging.Query, budget int) (botmessaging.Page, error) {
	page := botmessaging.Page{Bots: []botmessaging.Bot{}}
	if !validBotQuery(query) {
		return page, ErrInvalidInput
	}
	required, err := json.Marshal(append([]string{}, query.RequiredTools...))
	if err != nil {
		return page, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,name,description,category,tool_permissions,status,accept_dm,can_initiate FROM (
 SELECT b.id,b.name,b.description,b.category,b.tool_permissions,`+botStatusSQL+` AS status,(b.messaging_policy->>'accept_dm')::boolean AS accept_dm,(b.messaging_policy->>'can_initiate')::boolean AS can_initiate
 FROM tombot.bots b WHERE b.user_id=$1 AND ($2='' OR b.id>$2) AND ($3='' OR b.id=$3) AND b.tool_permissions @> $4::jsonb
 ) directory WHERE (NOT $5 OR (status='idle' AND id<>$6)) ORDER BY id LIMIT $7`, user, query.After, query.BotID, string(required), query.OnlyIdle, self, botmessaging.PageSize+1)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var bot botmessaging.Bot
		var permissions []byte
		if err = rows.Scan(&bot.ID, &bot.Name, &bot.Description, &bot.Category, &permissions, &bot.Status, &bot.AcceptDM, &bot.CanInitiate); err != nil {
			return page, errors.Join(err, rows.Close())
		}
		if err = json.Unmarshal(permissions, &bot.Tools); err != nil {
			return page, errors.Join(err, rows.Close())
		}
		page.Bots = append(page.Bots, bot)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return page, err
	}
	if len(page.Bots) > botmessaging.PageSize {
		page.Bots = page.Bots[:botmessaging.PageSize]
		page.Next = page.Bots[len(page.Bots)-1].ID
	}
	for {
		encoded, err := json.Marshal(page)
		if err != nil {
			return page, err
		}
		if len(encoded) <= budget {
			return page, nil
		}
		if len(page.Bots) <= 1 {
			return page, ErrContextLimit
		}
		page.Bots = page.Bots[:len(page.Bots)-1]
		page.Next = page.Bots[len(page.Bots)-1].ID
	}
}

// botDirectoryContext 首次启动固定目录快照，恢复重用相同输入；工具查询提供更新后的状态。
func botDirectoryContext(ctx context.Context, tx *sql.Tx, lease Lease, budget int) (string, error) {
	var saved *string
	if err := tx.QueryRowContext(ctx, `SELECT bot_directory_context FROM tombot.runs WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID).Scan(&saved); err != nil {
		return "", err
	}
	if saved != nil {
		return *saved, nil
	}
	page, err := listBotDirectory(ctx, tx, lease.UserID, lease.BotID, botmessaging.Query{}, budget)
	if err != nil && !errors.Is(err, ErrContextLimit) {
		return "", err
	}
	var snapshot any = page
	if errors.Is(err, ErrContextLimit) {
		snapshot = map[string]string{"lookup_required": "list_bots", "reason": "The directory exceeds this run's snapshot budget. Query current cats with pagination before choosing a peer."}
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	result := string(encoded)
	_, err = tx.ExecContext(ctx, `UPDATE tombot.runs SET bot_directory_context=$3 WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID, result)
	return result, err
}

func hasRequiredTools(bot botmessaging.Bot, required []string) bool {
	for _, name := range required {
		if !slices.Contains(bot.Tools, name) {
			return false
		}
	}
	return true
}
