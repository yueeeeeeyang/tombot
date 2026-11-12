package domain

import "time"

type ConversationSearch struct {
	Query string `json:"query"`
	BotID string `json:"bot_id"`
	Kind  string `json:"kind"`
	Limit int    `json:"limit"`
}
type ConversationHit struct {
	ConversationID string    `json:"conversation_id"`
	Title          string    `json:"title"`
	Kind           string    `json:"kind"`
	MessageID      string    `json:"message_id"`
	Snippet        string    `json:"snippet"`
	CreatedAt      time.Time `json:"created_at"`
}
type ConversationRead struct {
	ConversationID  string `json:"conversation_id"`
	AroundMessageID string `json:"around_message_id"`
	Limit           int    `json:"limit"`
}
type TranscriptMessage struct {
	ID          string    `json:"id"`
	Seq         int64     `json:"seq"`
	SenderKind  string    `json:"sender_kind"`
	SenderBotID *string   `json:"sender_bot_id"`
	Text        string    `json:"text"`
	CreatedAt   time.Time `json:"created_at"`
}
type ConversationTranscript struct {
	ConversationID string              `json:"conversation_id"`
	Title          string              `json:"title"`
	Kind           string              `json:"kind"`
	Messages       []TranscriptMessage `json:"messages"`
	HasBefore      bool                `json:"has_before"`
	HasAfter       bool                `json:"has_after"`
}
