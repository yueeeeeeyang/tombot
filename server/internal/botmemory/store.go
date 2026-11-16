package botmemory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

const columns = `key,category,summary,content,revision,source_message_id,is_deleted`

// Search 使用字面子串匹配支持中文，主键前缀把扫描范围限制在当前猫咪的有限记忆内。
// 空查询可分页浏览；删除标记只返回键和版本，不返回被遗忘的正文。
func Search(ctx context.Context, tx *sql.Tx, userID, botID string, query Query) (page Page, result error) {
	if !ValidText(query.Query, 0, 120) || query.Offset < 0 || query.Offset > MaxEntries {
		return Page{}, ErrInput
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+columns+` FROM tombot.bot_memories WHERE user_id=$1 AND bot_id=$2
		AND ($3='' OR strpos(lower(key||' '||summary||' '||content),lower($3))>0) ORDER BY key LIMIT $4 OFFSET $5`, userID, botID, query.Query, PageSize+1, query.Offset)
	if err != nil {
		return Page{}, err
	}
	defer func() { result = errors.Join(result, rows.Close()) }()
	page.Entries = make([]Entry, 0, PageSize)
	for rows.Next() {
		var entry Entry
		if err = rows.Scan(&entry.Key, &entry.Category, &entry.Summary, &entry.Content, &entry.Revision, &entry.SourceMessageID, &entry.IsDeleted); err != nil {
			return Page{}, err
		}
		if len(page.Entries) == PageSize {
			next := query.Offset + PageSize
			page.NextOffset = &next
			break
		}
		page.Entries = append(page.Entries, entry)
	}
	return page, rows.Err()
}

// Snapshot 只注入有限摘要；优先近期非项目偏好，超出预算的内容仍能通过检索读取。
// 调用者把结果固定到运行记录，使恢复不会因其他回合更新记忆而改变模型输入。
func Snapshot(ctx context.Context, tx *sql.Tx, userID, botID string, maximum int) (snapshot string, result error) {
	if maximum < 128 || maximum > ContextBytes {
		return "", ErrInput
	}
	rows, err := tx.QueryContext(ctx, `SELECT key,category,summary,revision FROM tombot.bot_memories WHERE user_id=$1 AND bot_id=$2 AND NOT is_deleted
		ORDER BY (category='project'),updated_at DESC,key LIMIT $3`, userID, botID, MaxEntries)
	if err != nil {
		return "", err
	}
	defer func() { result = errors.Join(result, rows.Close()) }()
	type summary struct {
		Key      string `json:"key"`
		Category string `json:"category"`
		Summary  string `json:"summary"`
		Revision int64  `json:"revision"`
	}
	index := struct {
		Entries   []summary `json:"entries"`
		Truncated bool      `json:"truncated"`
	}{Entries: []summary{}}
	encoded := []byte(`{"entries":[],"truncated":false}`)
	for rows.Next() {
		var entry summary
		if err = rows.Scan(&entry.Key, &entry.Category, &entry.Summary, &entry.Revision); err != nil {
			return "", err
		}
		index.Entries = append(index.Entries, entry)
		candidate, err := json.Marshal(index)
		if err != nil {
			return "", err
		}
		if len(candidate) > maximum {
			index.Entries = index.Entries[:len(index.Entries)-1]
			index.Truncated = true
			encoded, err = json.Marshal(index)
			if err != nil {
				return "", err
			}
			break
		}
		encoded = candidate
	}
	if len(index.Entries) == 0 && !index.Truncated {
		return "", rows.Err()
	}
	return string(encoded), rows.Err()
}

// Apply 在调用方事务内执行带版本检查的写入或遗忘；锁定猫咪行，串行化容量检查和遗忘水位。
// Source 必须已核验为当前回合的用户原话。版本冲突、旧来源及容量超限均明确拒绝。
func Apply(ctx context.Context, tx *sql.Tx, userID, botID string, change Change, source Source, deleting bool) (any, error) {
	if err := change.Validate(deleting); err != nil {
		return nil, err
	}
	var forgottenBefore int64
	if err := tx.QueryRowContext(ctx, `SELECT memory_forget_sequence FROM tombot.bots WHERE user_id=$1 AND id=$2 FOR UPDATE`, userID, botID).Scan(&forgottenBefore); err != nil {
		return nil, err
	}
	if source.Sequence <= forgottenBefore || source.MessageID != change.SourceMessageID {
		return nil, ErrSource
	}
	if deleting && change.Key == "*" {
		removed, err := tx.ExecContext(ctx, `DELETE FROM tombot.bot_memories WHERE user_id=$1 AND bot_id=$2`, userID, botID)
		if err != nil {
			return nil, err
		}
		count, err := removed.RowsAffected()
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tombot.bots SET memory_forget_sequence=$3 WHERE user_id=$1 AND id=$2`, userID, botID, source.Sequence); err != nil {
			return nil, err
		}
		return map[string]any{"forgotten": count, "scope": "all memories of this cat", "chat_history_deleted": false}, nil
	}
	var revision, previousSequence int64
	var wasDeleted bool
	var category string
	err := tx.QueryRowContext(ctx, `SELECT revision,source_sequence,is_deleted,category FROM tombot.bot_memories WHERE user_id=$1 AND bot_id=$2 AND key=$3`, userID, botID, change.Key).Scan(&revision, &previousSequence, &wasDeleted, &category)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if revision != change.ExpectedRevision || deleting && revision == 0 {
		return nil, ErrConflict
	}
	if source.Sequence < previousSequence || wasDeleted && source.Sequence <= previousSequence {
		return nil, ErrSource
	}
	if revision == 0 {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tombot.bot_memories WHERE user_id=$1 AND bot_id=$2`, userID, botID).Scan(&count); err != nil {
			return nil, err
		}
		if count >= MaxEntries {
			return nil, ErrCapacity
		}
	}
	if deleting {
		change.Category = category
		change.Summary = ""
		change.Content = ""
	}
	entry := Entry{Key: change.Key, Category: change.Category, Summary: change.Summary, Content: change.Content, Revision: revision + 1, SourceMessageID: source.MessageID, IsDeleted: deleting}
	_, err = tx.ExecContext(ctx, `INSERT INTO tombot.bot_memories(user_id,bot_id,key,category,summary,content,revision,source_message_id,source_sequence,is_deleted)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(user_id,bot_id,key) DO UPDATE SET category=excluded.category,summary=excluded.summary,content=excluded.content,
		revision=excluded.revision,source_message_id=excluded.source_message_id,source_sequence=excluded.source_sequence,is_deleted=excluded.is_deleted,updated_at=clock_timestamp()`,
		userID, botID, entry.Key, entry.Category, entry.Summary, entry.Content, entry.Revision, entry.SourceMessageID, source.Sequence, deleting)
	return entry, err
}
