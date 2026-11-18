// Package execution 持久化用户消息、回合、运行和受租约保护的操作记录。
package execution

import (
	"encoding/json"
	"time"

	"io.yueyang/tom/internal/botmessaging"
	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/chatlibrary"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/groupchat"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
)

type SendRequest struct {
	FileIDs         []string `json:"file_ids,omitempty"`
	ClientRequestID string   `json:"client_request_id"`
	Content         string   `json:"content"`
	ReplyToID       string   `json:"reply_to_id,omitempty"`
	Mentions        []string `json:"mentions,omitempty"`
}
type Submission struct {
	TurnID    string `json:"turn_id"`
	RunID     string `json:"run_id"`
	MessageID string `json:"message_id"`
	IsCreated bool   `json:"is_created"`
}
type Block struct {
	DMNotice    *botmessaging.Notice    `json:"dm_notice,omitempty"`
	UserMessage bool                    `json:"user_message,omitempty"`
	SourceRunID string                  `json:"source_run_id,omitempty"`
	ChatFile    *chatlibrary.File       `json:"chat_file,omitempty"`
	BotMessage  *botmessaging.Reference `json:"bot_message,omitempty"`
	Mentions    []groupchat.Mention     `json:"mentions,omitempty"`
	TaskResult  bool                    `json:"task_result,omitempty"`
	Kind        string                  `json:"kind"`
	Text        string                  `json:"text"`
	Reaction    *ReactionChange         `json:"reaction,omitempty"`
}

// ReactionChange 记录一次不可变的表情交互，目标由消息的 reply_to_id 绑定。
type ReactionChange struct {
	Emoji    string `json:"emoji"`
	IsActive bool   `json:"is_active"`
}

type ReactionRequest struct {
	ClientRequestID string `json:"client_request_id"`
	Emoji           string `json:"emoji"`
	IsActive        bool   `json:"is_active"`
}

type Reaction struct {
	Emoji string `json:"emoji"`
	Count int    `json:"count"`
}
type Message struct {
	PrivateMessage   bool          `json:"-"`
	GroupMember      bool          `json:"-"`
	OwnMessage       bool          `json:"-"`
	ID               string        `json:"id"`
	ClientRequestID  string        `json:"client_request_id,omitempty"`
	ConversationID   string        `json:"conversation_id"`
	TurnID           string        `json:"turn_id"`
	RunID            *string       `json:"run_id"`
	Sequence         int64         `json:"sequence"`
	SenderKind       string        `json:"sender_kind"`
	SenderBotID      *string       `json:"sender_bot_id"`
	Blocks           []Block       `json:"blocks"`
	Status           string        `json:"status"`
	CreatedAt        time.Time     `json:"created_at"`
	ReplyTo          *MessageReply `json:"reply_to,omitempty"`
	ReactionRevision int64         `json:"reaction_revision"`
	Reactions        []Reaction    `json:"reactions"`
}

// MessageReply 的摘要供界面使用；原文只在服务端组装模型上下文，不递归展开引用。
type MessageReply struct {
	MessageID  string `json:"message_id"`
	Excerpt    string `json:"excerpt"`
	senderKind string
	blocks     []Block
}
type Run struct {
	ID                 string     `json:"id"`
	TurnID             string     `json:"turn_id"`
	ConversationID     string     `json:"conversation_id"`
	BotID              string     `json:"bot_id"`
	Status             string     `json:"status"`
	ErrorCode          string     `json:"error_code"`
	ComputerWaitReason string     `json:"computer_wait_reason"`
	ProgressSummary    string     `json:"progress_summary"`
	CreatedAt          time.Time  `json:"created_at"`
	StartedAt          *time.Time `json:"started_at"`
	FinishedAt         *time.Time `json:"finished_at"`
}

// Lease 只在受信 Worker 内使用；用户接口不能接收或自行构造调度身份。
type Lease struct {
	UserID         string
	JobID          string
	RunID          string
	BotID          string
	ConversationID string
	TurnID         string
	OwnerID        string
	FencingToken   int64
	Attempt        int
	ExpiresAt      time.Time
}

// Work 保存运行开始时固定的机器人设定与模型选择，以及有界上下文和已有操作结果。
type Work struct {
	IsConsole         bool
	Run               Run
	Bot               bots.Spec
	Selection         *modelregistry.Selection
	Messages          []Message
	Operations        []Operation
	HistoryTruncated  bool
	ContinuationAfter int64
	TriggerSequence   int64
	MemoryContext     string
	HasMemory         bool
	IsTask            bool
	GroupContext      string
	BotDirectory      string
	PrivateContext    string
}
type Operation struct {
	ID            string          `json:"id"`
	Sequence      int64           `json:"sequence"`
	StepKey       string          `json:"step_key"`
	Kind          string          `json:"kind"`
	Status        string          `json:"status"`
	Result        json.RawMessage `json:"result"`
	ErrorCode     string          `json:"error_code"`
	Attempt       int             `json:"attempt"`
	FencingToken  int64           `json:"fencing_token"`
	RequestDigest []byte          `json:"-"`
}
type OperationRequest struct {
	StepKey       string
	Kind          string
	RequestDigest []byte
	Model         *ModelCall
	ToolName      string
	// Computer 绑定当前 Agent 执行权；Worker 的模型及所有工具操作都传入，电脑操作另保存会话外键。
	Computer *computer.ExecutionSession
}

// ModelCall 必须由受信 Worker 用固定版本配置构造，覆盖整个外部调用的硬时限。
type ModelCall struct {
	Configuration modelregistry.Configuration
	Lifetime      time.Duration
}
type OperationResult struct {
	Status     string
	Result     json.RawMessage
	ErrorCode  string
	Usage      *provider.ReportedUsage
	UsageFinal bool
	Screenshot *domain.ArtifactRef
}
type Outcome struct {
	Status    string
	Answer    string
	ErrorCode string
}
