package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/store"
)

const maxTranscriptBytes = 1 << 20

// conversationParticipant 由当前成员关系定义访问权；过滤 Bot 只能进一步缩小结果，不能替换调用身份。
const conversationParticipant = `((c.kind='direct' AND c.bot_id=?) OR (c.kind='bot_dm' AND (c.bot_id=? OR c.initiator_bot_id=?)) OR (c.kind='group' AND EXISTS(SELECT 1 FROM group_members gm WHERE gm.group_id=c.group_id AND gm.bot_id=?)))`

func participantArgs(botID string) []any { return []any{botID, botID, botID, botID} }

func validConversationID(id string) bool {
	return id != "" && len(id) <= 128 && strings.TrimSpace(id) == id && !strings.ContainsRune(id, 0) && utf8.ValidString(id)
}

// CheckConversationAccess 对不存在和不参与的会话返回同一个错误，避免通过标识枚举他人会话。
func CheckConversationAccess(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, botID, conversationID string) error {
	if !validConversationID(botID) || !validConversationID(conversationID) {
		return domain.NewError(403, "CONVERSATION_FORBIDDEN", "You may only read conversations you participate in.")
	}
	var allowed bool
	args := append([]any{conversationID}, participantArgs(botID)...)
	if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM conversations c WHERE c.id=? AND "+conversationParticipant+")", args...).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return domain.NewError(403, "CONVERSATION_FORBIDDEN", "You may only read conversations you participate in.")
	}
	return nil
}

// ValidateConversationSearch 返回字面检索词项，避免把外部文本直接解释为 FTS 语法。
func ValidateConversationSearch(input domain.ConversationSearch) ([]string, error) {
	terms := strings.FieldsFunc(input.Query, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if !utf8.ValidString(input.Query) || strings.ContainsRune(input.Query, 0) || len(input.Query) > 512 || len(terms) < 1 || len(terms) > 16 || input.Limit < 0 || input.Limit > 50 || input.BotID != "" && !validConversationID(input.BotID) || input.Kind != "" && input.Kind != "direct" && input.Kind != "group" && input.Kind != "bot_dm" {
		return nil, domain.NewError(400, "CONVERSATION_QUERY_INVALID", "Use 1 to 16 literal terms within 512 UTF-8 bytes, a valid conversation kind, and a limit from 1 to 50.")
	}
	return terms, nil
}

// SearchTranscript 在 SQL 限额前检查参与身份，只返回公开发言片段；失败时不返回未授权或部分结果。
func (s *Conversations) SearchTranscript(ctx context.Context, botID string, input domain.ConversationSearch) ([]domain.ConversationHit, error) {
	terms, err := ValidateConversationSearch(input)
	if err != nil {
		return nil, err
	}
	if !validConversationID(botID) {
		return nil, domain.NewError(403, "CONVERSATION_FORBIDDEN", "A valid calling bot is required.")
	}
	if input.Limit == 0 {
		input.Limit = 10
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	quoted := make([]string, 0, len(terms))
	short := false
	for _, term := range terms {
		quoted = append(quoted, `"`+term+`"`)
		short = short || utf8.RuneCountInString(term) < 3
	}
	match := "messages_fts MATCH ?"
	rank := "bm25(messages_fts),m.created_at DESC,m.id DESC"
	snippet := "snippet(messages_fts,2,'','',' … ',48)"
	args := []any{strings.Join(quoted, " AND ")}
	if short {
		clauses := []string{}
		args = nil
		for _, term := range terms {
			clauses = append(clauses, "f.content LIKE ? ESCAPE '\\'")
			args = append(args, "%"+strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(term)+"%")
		}
		match = strings.Join(clauses, " AND ")
		rank = "m.created_at DESC,m.id DESC"
		snippet = "substr(f.content,MAX(1,instr(lower(f.content),lower(?))-60),240)"
		args = append([]any{terms[0]}, args...)
	}
	where := match + " AND m.status!='streaming' AND " + conversationParticipant
	args = append(args, participantArgs(botID)...)
	if input.BotID != "" {
		where += " AND " + conversationParticipant
		args = append(args, participantArgs(input.BotID)...)
	}
	if input.Kind != "" {
		where += " AND c.kind=?"
		args = append(args, input.Kind)
	}
	args = append(args, input.Limit)
	hits := []domain.ConversationHit{}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT c.id,c.title,c.kind,m.id,`+snippet+`,m.created_at FROM messages_fts f JOIN messages m ON m.id=f.message_id JOIN conversations c ON c.id=m.conversation_id WHERE `+where+` ORDER BY `+rank+` LIMIT ?`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var hit domain.ConversationHit
			var created int64
			if err = rows.Scan(&hit.ConversationID, &hit.Title, &hit.Kind, &hit.MessageID, &hit.Snippet, &created); err != nil {
				return err
			}
			hit.CreatedAt = time.UnixMilli(created).UTC()
			characters := []rune(hit.Snippet)
			if len(characters) > 480 {
				hit.Snippet = string(characters[:480]) + "…"
			}
			hits = append(hits, hit)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		return store.Audit(ctx, tx, "bot:"+botID, "conversation.search", botID, map[string]any{"filter_bot_id": input.BotID, "kind": input.Kind, "hits": len(hits)})
	})
	if err != nil {
		return nil, err
	}
	return hits, nil
}

// ReadTranscript 返回按序排列的公开正文窗口，锚点跨会话时拒绝；超过容量要求缩小窗口，不静默截断正文。
func (s *Conversations) ReadTranscript(ctx context.Context, botID string, input domain.ConversationRead) (domain.ConversationTranscript, error) {
	transcript := domain.ConversationTranscript{Messages: []domain.TranscriptMessage{}}
	if input.Limit == 0 {
		input.Limit = 30
	}
	if input.Limit < 1 || input.Limit > 100 || input.AroundMessageID != "" && !validConversationID(input.AroundMessageID) {
		return transcript, badInput("Use a limit from 1 to 100 and a valid message ID.")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if err := CheckConversationAccess(ctx, tx, botID, input.ConversationID); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, "SELECT id,title,kind FROM conversations WHERE id=?", input.ConversationID).Scan(&transcript.ConversationID, &transcript.Title, &transcript.Kind); err != nil {
			return err
		}
		var start int64
		if input.AroundMessageID != "" {
			var anchor int64
			if err := tx.QueryRowContext(ctx, "SELECT seq FROM messages WHERE id=? AND conversation_id=? AND status!='streaming'", input.AroundMessageID, input.ConversationID).Scan(&anchor); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return domain.NewError(404, "CONVERSATION_MESSAGE_NOT_FOUND", "The anchor message is not a completed message in this conversation.")
				}
				return err
			}
			if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MIN(seq),0) FROM (SELECT seq FROM messages WHERE conversation_id=? AND seq<=? AND status!='streaming' ORDER BY seq DESC LIMIT ?)", input.ConversationID, anchor, input.Limit/2+1).Scan(&start); err != nil {
				return err
			}
		} else {
			if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MIN(seq),0) FROM (SELECT seq FROM messages WHERE conversation_id=? AND status!='streaming' ORDER BY seq DESC LIMIT ?)", input.ConversationID, input.Limit).Scan(&start); err != nil {
				return err
			}
		}
		page := ` FROM messages m JOIN message_transcripts f ON f.message_id=m.id WHERE m.conversation_id=? AND m.seq>=? AND m.status!='streaming' ORDER BY m.seq LIMIT ?`
		var size int64
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(size),0) FROM (SELECT length(CAST(f.content AS BLOB)) AS size"+page+")", input.ConversationID, start, input.Limit).Scan(&size); err != nil {
			return err
		}
		if size > maxTranscriptBytes {
			return domain.NewError(413, "CONVERSATION_TOO_LARGE", "This context exceeds one MiB. Read a smaller window around a returned message ID.")
		}
		rows, err := tx.QueryContext(ctx, "SELECT m.id,m.seq,m.sender_kind,m.sender_bot_id,f.content,m.created_at"+page, input.ConversationID, start, input.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var message domain.TranscriptMessage
			var created int64
			if err = rows.Scan(&message.ID, &message.Seq, &message.SenderKind, &message.SenderBotID, &message.Text, &created); err != nil {
				return err
			}
			message.CreatedAt = time.UnixMilli(created).UTC()
			transcript.Messages = append(transcript.Messages, message)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(transcript.Messages) > 0 {
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE conversation_id=? AND seq<? AND status!='streaming'),EXISTS(SELECT 1 FROM messages WHERE conversation_id=? AND seq>? AND status!='streaming')`, input.ConversationID, transcript.Messages[0].Seq, input.ConversationID, transcript.Messages[len(transcript.Messages)-1].Seq).Scan(&transcript.HasBefore, &transcript.HasAfter); err != nil {
				return err
			}
		}
		return store.Audit(ctx, tx, "bot:"+botID, "conversation.read", input.ConversationID, map[string]any{"around_message_id": input.AroundMessageID, "messages": len(transcript.Messages)})
	})
	if err != nil {
		return domain.ConversationTranscript{}, err
	}
	return transcript, nil
}
