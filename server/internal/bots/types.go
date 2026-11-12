// Package bots 管理当前用户的机器人、群组与会话目录，不执行模型或宿主机操作。
package bots

import "time"

// MessagingPolicy 控制机器人私信、主动发言与群邀请，执行时仍须复核双方权限。
type MessagingPolicy struct {
	AcceptDM          bool `json:"accept_dm"`
	CanInitiate       bool `json:"can_initiate"`
	AcceptGroupInvite bool `json:"accept_group_invite"`
}

// Spec 不包含模型选择、供应商凭据或容器路径；AvatarRef 仅是客户端展示资产的标识。
type Spec struct {
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	AvatarRef       string          `json:"avatar_ref"`
	Category        string          `json:"category"`
	SystemPrompt    string          `json:"system_prompt"`
	ToolPermissions []string        `json:"tool_permissions"`
	ApprovalMode    string          `json:"approval_mode"`
	MessagingPolicy MessagingPolicy `json:"messaging_policy"`
	AccentHue       int             `json:"accent_hue"`
}

// Bot 使用唯一业务身份，同时持有稳定桌面、唯一用户单聊及可比较的资源修订。
type Bot struct {
	ID string `json:"id"`
	Spec
	DesktopID       string    `json:"desktop_id"`
	ConversationID  string    `json:"conversation_id"`
	HasCustomAvatar bool      `json:"has_custom_avatar"`
	IsArchived      bool      `json:"is_archived"`
	Revision        int64     `json:"revision"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type CreateRequest struct {
	ClientRequestID string `json:"client_request_id"`
	Spec
}

type UpdateRequest struct {
	ExpectedRevision int64 `json:"expected_revision"`
	Spec
}

// GroupSpec 的成员为无重复身份集合，输出按稳定身份排序，不依赖客户端显示顺序。
type GroupSpec struct {
	Name         string   `json:"name"`
	Purpose      string   `json:"purpose"`
	MemberIDs    []string `json:"member_ids"`
	MaxCostMicro *int64   `json:"max_cost_micro"`
}

type Group struct {
	ID string `json:"id"`
	GroupSpec
	ConversationID string    `json:"conversation_id"`
	IsArchived     bool      `json:"is_archived"`
	Revision       int64     `json:"revision"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type CreateGroupRequest struct {
	ClientRequestID string `json:"client_request_id"`
	GroupSpec
}

type UpdateGroupRequest struct {
	ExpectedRevision int64 `json:"expected_revision"`
	GroupSpec
}

// Workspace 只保存当前用户的选择，不再整体覆盖机器人与群组集合。
type Workspace struct {
	ActiveBotID  *string `json:"active_bot_id"`
	PrimaryBotID *string `json:"primary_bot_id"`
	Revision     int64   `json:"revision"`
}

type WorkspaceRequest struct {
	ExpectedRevision int64   `json:"expected_revision"`
	ActiveBotID      *string `json:"active_bot_id"`
	PrimaryBotID     *string `json:"primary_bot_id"`
}

// Snapshot 的目录首页与事件水位来自同一个只读数据库快照。
type Snapshot struct {
	Workspace     Workspace `json:"workspace"`
	Bots          []Bot     `json:"bots"`
	Groups        []Group   `json:"groups"`
	HasMoreBots   bool      `json:"has_more_bots"`
	HasMoreGroups bool      `json:"has_more_groups"`
	EventSequence int64     `json:"event_sequence"`
}

type Conversation struct {
	ID               string    `json:"id"`
	Kind             string    `json:"kind"`
	BotID            *string   `json:"bot_id"`
	GroupID          *string   `json:"group_id"`
	PeerBotID        *string   `json:"peer_bot_id"`
	Title            string    `json:"title"`
	MessageSequence  int64     `json:"message_sequence"`
	LastReadSequence int64     `json:"last_read_sequence"`
	IsArchived       bool      `json:"is_archived"`
	Revision         int64     `json:"revision"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Avatar 为受控 PNG；内容仅从专用二进制接口返回，不混入目录快照与事件。
type Avatar struct {
	Content []byte
	SHA256  string
}
