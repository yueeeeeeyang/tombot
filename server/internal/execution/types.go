// Package execution 持久化用户消息、回合、运行和受租约保护的操作记录。
package execution

import (
	"encoding/json"
	"time"

	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
)

type SendRequest struct {
	ClientRequestID string `json:"client_request_id"`
	Content         string `json:"content"`
}
type Submission struct {
	TurnID    string `json:"turn_id"`
	RunID     string `json:"run_id"`
	MessageID string `json:"message_id"`
	IsCreated bool   `json:"is_created"`
}
type Block struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}
type Message struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	TurnID         string    `json:"turn_id"`
	RunID          *string   `json:"run_id"`
	Sequence       int64     `json:"sequence"`
	SenderKind     string    `json:"sender_kind"`
	SenderBotID    *string   `json:"sender_bot_id"`
	Blocks         []Block   `json:"blocks"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
}
type Run struct {
	ID                 string     `json:"id"`
	TurnID             string     `json:"turn_id"`
	ConversationID     string     `json:"conversation_id"`
	BotID              string     `json:"bot_id"`
	Status             string     `json:"status"`
	ErrorCode          string     `json:"error_code"`
	ComputerWaitReason string     `json:"computer_wait_reason"`
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
	Run              Run
	Bot              bots.Spec
	Selection        *modelregistry.Selection
	Messages         []Message
	Operations       []Operation
	HistoryTruncated bool
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
