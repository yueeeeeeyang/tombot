package service

import (
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"io.yueyang/tom/internal/domain"
)

const (
	maxAgents          = 64
	maxMessages        = 500
	maxMessageLength   = 10_000
	maxArtifacts       = 100
	maxArtifactTitle   = 80
	maxArtifactPreview = 16_000
	maxComputerURL     = 2_048
	maxNameLength      = 24
	maxDescription     = 120
	maxGroupLength     = 24
	replyExcerptLength = 80
	maxIDLength        = 128
)

var artifactPreviewPattern = regexp.MustCompile(`^data:image/(?:png|jpeg|webp);base64,[a-zA-Z0-9+/=]+$`)

var avatars = map[string]struct{}{
	"bombay": {}, "orange-tabby": {}, "british-shorthair": {}, "ragdoll": {},
	"siamese": {}, "american-shorthair": {}, "persian": {}, "calico": {},
}

var catalogModels = map[string]struct{}{
	"gpt-4o": {}, "claude-sonnet": {}, "deepseek-v3": {}, "gemini-flash": {},
}

var reactionEmojis = map[string]struct{}{
	"👍": {}, "👎": {}, "❤️": {}, "😂": {}, "🎉": {}, "😊": {}, "😍": {}, "🥰": {}, "😘": {}, "😅": {},
	"🤣": {}, "😭": {}, "😡": {}, "🤔": {}, "😴": {}, "😎": {}, "🥳": {}, "👏": {}, "🙏": {}, "🔥": {},
	"✨": {}, "💯": {}, "👀": {}, "✅": {}, "❌": {}, "🤝": {}, "💪": {}, "🌹": {}, "☕": {},
}

// Workspace 与桌面 v5 工作空间同一形状，字段名保持界面已使用的驼峰。
type Workspace struct {
	Version       int     `json:"version"`
	ActiveAgentID string  `json:"activeAgentId"`
	Agents        []Agent `json:"agents"`
}

// Agent 是侧栏里的一只猫咪。模型标识来自界面目录，不等于供应商的模型名称。
type Agent struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Group       string        `json:"group"`
	Avatar      string        `json:"avatar"`
	ModelID     string        `json:"modelId"`
	Messages    []ChatMessage `json:"messages"`
	Draft       string        `json:"draft"`
	Artifacts   []Artifact    `json:"artifacts"`
	ComputerURL string        `json:"computerUrl"`
}

// MessageAttachment 是气泡上的图片或文件。文件本身在工作区，这里只保留定位信息。
type MessageAttachment struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"mediaType"`
	Kind      string `json:"kind"`
}

// DmNotice 是消息流里的私信提示，不是猫咪代替用户发出的气泡。
type DmNotice struct {
	Direction      string `json:"direction"`
	PeerID         string `json:"peerId"`
	PeerName       string `json:"peerName"`
	ConversationID string `json:"conversationId"`
}

// ChatMessage 是气泡。未配置模型时保持 unsent，调用失败才是 failed。
type ChatMessage struct {
	ID          string              `json:"id"`
	Role        string              `json:"role"`
	Content     string              `json:"content"`
	CreatedAt   string              `json:"createdAt"`
	Status      string              `json:"status"`
	RequestID   string              `json:"requestId,omitempty"`
	ReplyTo     *Reply              `json:"replyTo,omitempty"`
	Reactions   []Reaction          `json:"reactions"`
	Attachments []MessageAttachment `json:"attachments,omitempty"`
	DmNotice    *DmNotice           `json:"dmNotice,omitempty"`
}

// Reply 保存被引用消息的标识和界面摘要。
type Reply struct {
	MessageID string `json:"messageId"`
	Excerpt   string `json:"excerpt"`
}

// Reaction 是气泡内的表情。同一种表情只记录一次。
type Reaction struct {
	Emoji string `json:"emoji"`
	Count int    `json:"count"`
}

// Artifact 是资料库卡片。应用不会编造文件，只保存已经写入的条目。
type Artifact struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Kind      string `json:"kind"`
	CreatedAt string `json:"createdAt"`
	Preview   string `json:"preview,omitempty"`
}

// ModelForm 是设置页里除主题以外的选项。密钥不在这份 JSON 里。
type ModelForm struct {
	Language        string  `json:"language"`
	Provider        string  `json:"provider"`
	ProviderName    string  `json:"providerName"`
	BaseURL         string  `json:"baseUrl"`
	ModelName       string  `json:"modelName"`
	Temperature     float64 `json:"temperature"`
	TopP            float64 `json:"topP"`
	MaxOutputTokens string  `json:"maxOutputTokens"`
	IsStreaming     bool    `json:"isStreaming"`
}

// ModelFormView 是返回给界面的设置。hasApiKey 只说明是否保存过密钥。
type ModelFormView struct {
	ModelForm
	HasAPIKey bool `json:"hasApiKey"`
}

// ParseWorkspace 校验桌面提交的工作空间。历史迁移脚本不在这里执行。
func ParseWorkspace(raw []byte) (Workspace, error) {
	if len(raw) > 4<<20 {
		return Workspace{}, invalidWorkspace("工作空间超过 4 MiB")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var workspace Workspace
	if err := decoder.Decode(&workspace); err != nil {
		return Workspace{}, invalidWorkspace("工作空间格式无效")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Workspace{}, invalidWorkspace("工作空间格式无效")
	}
	if err := validateWorkspace(&workspace); err != nil {
		return Workspace{}, err
	}
	return workspace, nil
}

func validateWorkspace(workspace *Workspace) error {
	if workspace.Version != 5 {
		return invalidWorkspace("工作空间版本无效")
	}
	if workspace.Agents == nil {
		workspace.Agents = []Agent{}
	}
	if len(workspace.Agents) > maxAgents {
		return invalidWorkspace("助手数量无效")
	}
	if len(workspace.Agents) == 0 {
		if workspace.ActiveAgentID != "" {
			return invalidWorkspace("工作空间的助手标识无效")
		}
		return nil
	}
	if utf16Len(workspace.ActiveAgentID) == 0 || utf16Len(workspace.ActiveAgentID) > maxIDLength {
		return invalidWorkspace("工作空间的助手标识无效")
	}
	ids := make(map[string]struct{}, len(workspace.Agents))
	messageIDs := make(map[string]struct{})
	totalMessages := 0
	for index := range workspace.Agents {
		agent := &workspace.Agents[index]
		if err := validateAgent(agent, messageIDs); err != nil {
			return err
		}
		if _, exists := ids[agent.ID]; exists {
			return invalidWorkspace("工作空间的助手标识无效")
		}
		ids[agent.ID] = struct{}{}
		totalMessages += len(agent.Messages)
	}
	if totalMessages > maxMessages {
		return invalidWorkspace("本地消息数量已达上限")
	}
	if _, ok := ids[workspace.ActiveAgentID]; !ok {
		return invalidWorkspace("工作空间的助手标识无效")
	}
	return nil
}

func validateAgent(agent *Agent, messageIDs map[string]struct{}) error {
	agent.Name = strings.TrimSpace(agent.Name)
	agent.Description = strings.TrimSpace(agent.Description)
	agent.Group = strings.TrimSpace(agent.Group)
	if agent.Messages == nil || agent.Artifacts == nil {
		return invalidWorkspace("助手资料不完整")
	}
	if utf16Len(agent.ID) == 0 || utf16Len(agent.ID) > maxIDLength ||
		utf16Len(agent.Name) == 0 || utf16Len(agent.Name) > maxNameLength ||
		utf16Len(agent.Description) > maxDescription ||
		utf16Len(agent.Group) == 0 || utf16Len(agent.Group) > maxGroupLength ||
		utf16Len(agent.Draft) > maxMessageLength ||
		utf16Len(agent.ComputerURL) > maxComputerURL {
		return invalidWorkspace("助手资料无效")
	}
	if _, ok := avatars[agent.Avatar]; !ok {
		return invalidWorkspace("请选择预设猫咪头像")
	}
	if !validCatalogOrNamedModel(agent.ModelID) {
		return invalidWorkspace("请选择列表中的模型")
	}
	if !validComputerURL(agent.ComputerURL) {
		return invalidWorkspace("请输入不含登录凭据的 HTTP 或 HTTPS 地址。")
	}
	if len(agent.Messages) > maxMessages || len(agent.Artifacts) > maxArtifacts {
		return invalidWorkspace("助手资料超过容量")
	}
	artifactIDs := make(map[string]struct{}, len(agent.Artifacts))
	for _, artifact := range agent.Artifacts {
		if _, exists := artifactIDs[artifact.ID]; exists {
			return invalidWorkspace("产物标识不能重复")
		}
		artifactIDs[artifact.ID] = struct{}{}
		if err := validateArtifact(artifact); err != nil {
			return err
		}
	}
	localIDs := make(map[string]struct{}, len(agent.Messages))
	for index := range agent.Messages {
		message := &agent.Messages[index]
		if message.Reactions == nil {
			return invalidWorkspace("消息格式无效")
		}
		if _, exists := messageIDs[message.ID]; exists {
			return invalidWorkspace("消息标识重复")
		}
		messageIDs[message.ID] = struct{}{}
		localIDs[message.ID] = struct{}{}
		if err := validateMessage(message); err != nil {
			return err
		}
	}
	for _, message := range agent.Messages {
		if message.ReplyTo != nil && message.ReplyTo.MessageID == message.ID {
			return invalidWorkspace("消息不能回复自己")
		}
	}
	return nil
}

func validateMessage(message *ChatMessage) error {
	message.Content = strings.TrimSpace(message.Content)
	if utf16Len(message.ID) == 0 || utf16Len(message.ID) > maxIDLength ||
		utf16Len(message.Content) == 0 || utf16Len(message.Content) > maxMessageLength {
		return invalidWorkspace("消息格式无效")
	}
	if message.Role != "user" && message.Role != "assistant" {
		return invalidWorkspace("消息格式无效")
	}
	if message.Status != "unsent" && message.Status != "failed" && message.Status != "sent" {
		return invalidWorkspace("消息格式无效")
	}
	if message.RequestID != "" && utf16Len(message.RequestID) > maxIDLength {
		return invalidWorkspace("消息格式无效")
	}
	if _, err := time.Parse(time.RFC3339Nano, message.CreatedAt); err != nil {
		return invalidWorkspace("消息时间无效")
	}
	if message.ReplyTo != nil {
		excerpt := strings.TrimSpace(message.ReplyTo.Excerpt)
		if utf16Len(message.ReplyTo.MessageID) == 0 || utf16Len(excerpt) == 0 || utf16Len(excerpt) > replyExcerptLength {
			return invalidWorkspace("回复摘要无效")
		}
		message.ReplyTo.Excerpt = excerpt
	}
	seen := make(map[string]struct{}, len(message.Reactions))
	for _, reaction := range message.Reactions {
		if _, ok := reactionEmojis[reaction.Emoji]; !ok || reaction.Count < 1 || reaction.Count > 999 {
			return invalidWorkspace("不支持这个表情")
		}
		if _, exists := seen[reaction.Emoji]; exists {
			return invalidWorkspace("同一条消息不能重复记录同一个表情")
		}
		seen[reaction.Emoji] = struct{}{}
	}
	return nil
}

func validateArtifact(artifact Artifact) error {
	title := strings.TrimSpace(artifact.Title)
	if utf16Len(artifact.ID) == 0 || utf16Len(artifact.ID) > 80 ||
		utf16Len(title) == 0 || utf16Len(title) > maxArtifactTitle {
		return invalidWorkspace("产物资料无效")
	}
	if artifact.Kind != "image" && artifact.Kind != "video" && artifact.Kind != "file" {
		return invalidWorkspace("产物资料无效")
	}
	if _, err := time.Parse(time.RFC3339Nano, artifact.CreatedAt); err != nil {
		return invalidWorkspace("产物时间无效")
	}
	if artifact.Preview != "" && (utf16Len(artifact.Preview) > maxArtifactPreview || !artifactPreviewPattern.MatchString(artifact.Preview)) {
		return invalidWorkspace("产物预览无效")
	}
	return nil
}

func validCatalogOrNamedModel(id string) bool {
	if _, ok := catalogModels[id]; ok {
		return true
	}
	return id != "" && strings.TrimSpace(id) == id && utf16Len(id) <= 120 && !strings.ContainsAny(id, "\r\n")
}

func validComputerURL(value string) bool {
	if value == "" {
		return true
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}

func utf16Len(value string) int {
	return len(utf16.Encode([]rune(value)))
}

func invalidWorkspace(message string) error {
	return domain.NewError(400, "WORKSPACE_INVALID", message)
}

// SeedWorkspace 是历史上的八只样例猫咪。新工作空间不再调用它；测试仍用它构造一份完整数据。
func SeedWorkspace() Workspace {
	type profile struct {
		id, name, description, group, avatar string
	}
	profiles := []profile{
		{"assistant", "小黑", "你的日常搭档，陪你把想法变成现实。", "我的助手", "bombay"},
		{"developer", "游戏程序", "代码、逻辑和实现，交给我一起想。", "游戏开发", "orange-tabby"},
		{"producer", "游戏负责人 + 策划", "从一个好点子，到一份清晰的计划。", "游戏开发", "british-shorthair"},
		{"artist", "游戏美术", "一起寻找属于这个世界的视觉语言。", "游戏开发", "calico"},
		{"audio", "游戏音效", "让每一个场景，都有自己的声音。", "游戏开发", "siamese"},
		{"coding", "编程搭档", "聊聊代码，也聊聊下一步怎么做。", "未分组", "american-shorthair"},
		{"news", "资讯助手", "为你整理值得关注的新鲜事。", "未分组", "ragdoll"},
		{"organizer", "整理助手", "少一点杂乱，多一点井井有条。", "未分组", "persian"},
	}
	workspace := Workspace{Version: 5, ActiveAgentID: "assistant", Agents: make([]Agent, 0, len(profiles))}
	for _, item := range profiles {
		workspace.Agents = append(workspace.Agents, Agent{
			ID: item.id, Name: item.name, Description: item.description, Group: item.group,
			Avatar: item.avatar, ModelID: "gpt-4o", Messages: []ChatMessage{}, Draft: "",
			Artifacts: []Artifact{}, ComputerURL: "",
		})
	}
	return workspace
}

// DefaultModelForm 与桌面设置表单的初始值一致。
func DefaultModelForm() ModelForm {
	return ModelForm{
		Language: "system", Provider: "deepseek", ProviderName: "DeepSeek", BaseURL: "", ModelName: "",
		Temperature: 0.7, TopP: 1, MaxOutputTokens: "4096", IsStreaming: true,
	}
}

// ParseModelForm 校验设置表单。密钥由调用方单独处理，不能出现在设置 JSON 里。
func ParseModelForm(raw []byte) (ModelForm, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var settings ModelForm
	if err := decoder.Decode(&settings); err != nil {
		return ModelForm{}, domain.NewError(400, "SETTINGS_INVALID", "模型设置格式无效")
	}
	if err := validateModelForm(&settings); err != nil {
		return ModelForm{}, err
	}
	return settings, nil
}

func validateModelForm(settings *ModelForm) error {
	switch settings.Language {
	case "system", "zh-CN", "en":
	default:
		return domain.NewError(400, "SETTINGS_INVALID", "不支持这个语言选项")
	}
	switch settings.Provider {
	case "openai", "anthropic", "deepseek", "google", "custom":
	default:
		return domain.NewError(400, "SETTINGS_INVALID", "不支持这个模型供应商")
	}
	settings.BaseURL = strings.TrimSpace(settings.BaseURL)
	settings.ModelName = strings.TrimSpace(settings.ModelName)
	settings.ProviderName = strings.TrimSpace(settings.ProviderName)
	if utf16Len(settings.ProviderName) > 40 {
		return domain.NewError(400, "SETTINGS_INVALID", "提供商名称最多 40 个字")
	}
	if utf16Len(settings.BaseURL) > 500 || utf16Len(settings.ModelName) > 120 {
		return domain.NewError(400, "SETTINGS_INVALID", "模型设置超过长度限制")
	}
	if settings.BaseURL != "" && !validComputerURL(settings.BaseURL) {
		return domain.NewError(400, "SETTINGS_INVALID", "请输入不含登录凭据的 HTTP 或 HTTPS 地址。")
	}
	if settings.Temperature < 0 || settings.Temperature > 2 || settings.TopP < 0 || settings.TopP > 1 {
		return domain.NewError(400, "SETTINGS_INVALID", "模型参数超出范围")
	}
	tokens, ok := parseTokenLimit(settings.MaxOutputTokens)
	if !ok || tokens < 1 || tokens > 131072 {
		return domain.NewError(400, "SETTINGS_INVALID", "请输入 1～131072 之间的整数。")
	}
	return nil
}

func parseTokenLimit(value string) (int, bool) {
	if value == "" || len(value) > 6 {
		return 0, false
	}
	number := 0
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, false
		}
		number = number*10 + int(char-'0')
	}
	return number, true
}

func modelReady(settings ModelForm) bool {
	return settings.BaseURL != "" && settings.ModelName != ""
}
