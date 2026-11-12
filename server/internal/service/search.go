package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"io.yueyang/tom/internal/domain"
)

const searchTimeout = 3 * time.Second

type SearchItem struct {
	Kind             string  `json:"kind"`
	ID               string  `json:"id"`
	Title            string  `json:"title"`
	Preview          string  `json:"preview"`
	ConversationID   string  `json:"conversation_id"`
	BotID            *string `json:"bot_id,omitempty"`
	GroupID          *string `json:"group_id,omitempty"`
	ConversationKind string  `json:"conversation_kind"`
	Source           string  `json:"source"`
	MessageID        string  `json:"message_id,omitempty"`
	Seq              int64   `json:"seq,omitempty"`
	category         int
	created          int64
}

type SearchPage struct {
	Items      []SearchItem `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

type searchCursor struct {
	Version  int    `json:"version"`
	Query    string `json:"query"`
	Category int    `json:"category"`
	Created  int64  `json:"created"`
	ID       string `json:"id"`
}

// Search 检索管理员可见的活动会话，只投影公开正文；游标绑定字面查询并以不可变创建时间和标识分页。
func (s *Conversations) Search(ctx context.Context, query, cursor string, limit int) (SearchPage, error) {
	page := SearchPage{Items: []SearchItem{}}
	query = strings.TrimSpace(query)
	if !utf8.ValidString(query) || len(query) > 512 || utf8.RuneCountInString(query) < 1 || utf8.RuneCountInString(query) > 128 || strings.ContainsFunc(query, unicode.IsControl) {
		return page, domain.NewError(400, "SEARCH_QUERY_INVALID", "Use 1 to 128 characters within 512 UTF-8 bytes, without control characters.")
	}
	if limit < 1 || limit > 50 {
		return page, domain.NewError(400, "INVALID_LIMIT", "Search limit must be between 1 and 50.")
	}
	digest := sha256.Sum256([]byte(query))
	queryHash := hex.EncodeToString(digest[:])
	var after searchCursor
	if cursor != "" {
		if len(cursor) > 2048 {
			return page, invalidSearchCursor()
		}
		encoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(encoded, &after) != nil || after.Version != 1 || after.Query != queryHash || after.Category < 0 || after.Category > 2 || after.Created < 0 || !validConversationID(after.ID) {
			return page, invalidSearchCursor()
		}
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	pattern := "%" + strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(query) + "%"
	messageMatch := "messages_fts MATCH ?"
	messageSnippet := "snippet(messages_fts,2,'','',' … ',64)"
	messageArgs := []any{`"` + strings.ReplaceAll(query, `"`, `""`) + `"`}
	if utf8.RuneCountInString(query) < 3 {
		// trigram 不索引一、二字词；短词走转义后的字面扫描，并受同一查询超时限制。
		messageMatch = "f.content LIKE ? ESCAPE '\\'"
		messageSnippet = "substr(f.content,MAX(1,instr(lower(f.content),lower(?))-40),240)"
		messageArgs = []any{query, pattern}
	}
	// 不读取摘要、角色提示词或工具块，避免公开名称命中后顺带暴露无关的私有材料。
	statement := `WITH hits AS (
 SELECT 0 category,'bot' kind,b.id,b.name title,substr(b.role_title,1,240) preview,c.id conversation_id,
 b.id bot_id,NULL group_id,c.kind conversation_kind,c.source,'' message_id,0 seq,b.created_at created
 FROM bots b JOIN conversations c ON c.bot_id=b.id AND c.kind='direct' AND c.source!='routine'
 WHERE b.is_archived=0 AND c.is_archived=0 AND (b.name LIKE ? ESCAPE '\' OR b.role_title LIKE ? ESCAPE '\')
 UNION ALL
 SELECT 1,'conversation',c.id,c.title,'',c.id,c.bot_id,c.group_id,c.kind,c.source,'',0,c.created_at
 FROM conversations c WHERE c.is_archived=0 AND c.title LIKE ? ESCAPE '\'
 UNION ALL
 SELECT 2,'message',m.id,c.title,` + messageSnippet + `,c.id,c.bot_id,c.group_id,c.kind,c.source,m.id,m.seq,m.created_at
 FROM messages_fts f JOIN messages m ON m.id=f.message_id JOIN conversations c ON c.id=m.conversation_id
 WHERE ` + messageMatch + ` AND c.is_archived=0 AND m.status!='streaming'
 ) SELECT category,kind,id,title,preview,conversation_id,bot_id,group_id,conversation_kind,source,message_id,seq,created
 FROM hits WHERE (?='' OR category>? OR (category=? AND (created<? OR (created=? AND id<?))))
 ORDER BY category,created DESC,id DESC LIMIT ?`
	args := append([]any{pattern, pattern, pattern}, messageArgs...)
	args = append(args, cursor, after.Category, after.Category, after.Created, after.Created, after.ID, limit+1)
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return page, searchError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var item SearchItem
		if err := rows.Scan(&item.category, &item.Kind, &item.ID, &item.Title, &item.Preview, &item.ConversationID, &item.BotID, &item.GroupID, &item.ConversationKind, &item.Source, &item.MessageID, &item.Seq, &item.created); err != nil {
			return SearchPage{}, searchError(ctx, err)
		}
		characters := []rune(item.Preview)
		if len(characters) > 240 {
			item.Preview = string(characters[:240]) + "…"
		}
		page.Items = append(page.Items, item)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return SearchPage{}, searchError(ctx, err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		encoded, err := json.Marshal(searchCursor{Version: 1, Query: queryHash, Category: last.category, Created: last.created, ID: last.ID})
		if err != nil {
			return SearchPage{}, err
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return page, nil
}

func invalidSearchCursor() error {
	return domain.NewError(400, "SEARCH_CURSOR_INVALID", "Use the cursor returned for the same search query.")
}

func searchError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return domain.NewError(504, "SEARCH_TIMEOUT", "The search timed out. Narrow the query and retry.")
	}
	return err
}
