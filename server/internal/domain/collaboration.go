package domain

import "context"

const MaxMessageHops = 4
const MaxMessageContextFiles = 8
const MaxMessageBytes = 32 << 10
const MaxMessageContextBytes = 32 << 10
const MaxMessageArtifacts = 32

type ContextFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
}
type BotMessageRequest struct {
	TargetBinding string        `json:"-"`
	BotID         string        `json:"bot_id"`
	Message       string        `json:"message"`
	ReplyRequired bool          `json:"reply_required"`
	Delegation    bool          `json:"delegation"`
	ContextFiles  []ContextFile `json:"context_files"`
}
type BotMessageReply struct {
	ConversationID string   `json:"conversation_id"`
	RunID          string   `json:"run_id"`
	Delivered      bool     `json:"delivered"`
	Summary        string   `json:"summary,omitempty"`
	Artifacts      []string `json:"artifacts"`
}

// Messenger 是单次工具调用持有的执行端口，避免工具注册表依赖可变的全局运行时。
type Messenger interface {
	SendBotMessage(context.Context, Run, string, BotMessageRequest) (BotMessageReply, error)
}
type ToolInvocation struct {
	Run        Run
	Bot        Bot
	Step       int
	Messenger  Messenger
	UserWaiter UserWaiter
}
type HandoffRef struct {
	ErrorCode      string `json:"error_code,omitempty"`
	FromBotID      string `json:"from_bot_id"`
	ToBotID        string `json:"to_bot_id"`
	ConversationID string `json:"conversation_id"`
	RunID          string `json:"run_id"`
	Reason         string `json:"reason"`
	HopDepth       int    `json:"hop_depth"`
}
