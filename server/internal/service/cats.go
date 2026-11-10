package service

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"io.yueyang/tom/internal/domain"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/secret"
)

const apiKeyRecord = "desktop-settings"

// Service 保存桌面工作空间的界面字段，并把发送交给原来的 Bot 与 agent 运行时。
type Service struct {
	mu           sync.Mutex
	db           *sql.DB
	key          []byte
	logger       *slog.Logger
	now          func() time.Time
	bots         *Bots
	catalog      *Catalog
	secrets      *secret.Manager
	workspaceDir string
	tickets      map[string]computerTicket
}

// Dependencies 是工作空间服务的显式依赖。执行时复用 Bot、目录和密钥，不再自带模型客户端。
type Dependencies struct {
	DB           *sql.DB
	Key          []byte
	Logger       *slog.Logger
	Now          func() time.Time
	Bots         *Bots
	Catalog      *Catalog
	Secrets      *secret.Manager
	WorkspaceDir string
}

// Snapshot 是一次读取到的工作空间。Pristine 只表示还没有用户改动。
type Snapshot struct {
	Workspace Workspace `json:"workspace"`
	Pristine  bool      `json:"pristine"`
	Revision  int64     `json:"revision"`
}

// Delivery 是投递结果。模型未配置时不改消息，调用失败会把该条标为 failed。
type Delivery struct {
	Workspace Workspace `json:"workspace"`
	Code      string    `json:"code,omitempty"`
	Message   string    `json:"message,omitempty"`
}

type stateRow struct {
	workspace  Workspace
	revision   int64
	pristine   bool
	settings   ModelForm
	nonce      []byte
	ciphertext []byte
}

// New 组装工作空间服务。没有数据库时直接失败，不用空实现假装可用。
func New(dep Dependencies) (*Service, error) {
	if dep.DB == nil {
		return nil, errors.New("工作空间服务缺少数据库")
	}
	logger := dep.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	now := dep.Now
	if now == nil {
		now = time.Now
	}
	return &Service{db: dep.DB, key: dep.Key, logger: logger, now: now, bots: dep.Bots, catalog: dep.Catalog, secrets: dep.Secrets, workspaceDir: dep.WorkspaceDir, tickets: map[string]computerTicket{}}, nil
}

type computerTicket struct {
	agentID string
	botID   string
	expires time.Time
}

// OpenComputer 确保猫咪对应原来的 Bot，并返回可嵌入的电脑页。手动地址仍可由界面另存。
func (s *Service) OpenComputer(ctx context.Context, agentID, base string) (string, error) {
	s.mu.Lock()
	row, err := s.ensureState(ctx)
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	var agent Agent
	found := false
	for _, item := range row.workspace.Agents {
		if item.ID == agentID {
			agent = item
			found = true
			break
		}
	}
	if !found {
		return "", domain.NewError(404, "AGENT_NOT_FOUND", "助手不存在")
	}
	modelID := ""
	if modelReady(row.settings) {
		apiKey, keyErr := s.openAPIKey(row)
		if keyErr != nil {
			return "", keyErr
		}
		defer clearBytes(apiKey)
		modelID, err = s.modelForAgent(ctx, row.settings, string(apiKey), agent.ModelID)
		if err != nil {
			return "", err
		}
	}
	if _, err = s.ensureBot(ctx, agent, modelID); err != nil {
		return "", err
	}
	botID, err := s.BotID(ctx, agentID)
	if err != nil {
		return "", err
	}
	ticket, err := secure.Token()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.tickets[ticket] = computerTicket{agentID: agentID, botID: botID, expires: s.now().Add(30 * time.Minute)}
	s.mu.Unlock()
	return base + "/api/v1/computer/view?agent=" + agentID + "&ticket=" + ticket, nil
}

// ComputerAccess 校验电脑页票据。票据只放在地址里，不写入日志。
func (s *Service) ComputerAccess(agentID, ticket string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.tickets[ticket]
	if !ok || entry.agentID != agentID || s.now().After(entry.expires) {
		return "", domain.NewError(401, "COMPUTER_TICKET_INVALID", "电脑连接已失效，请重新连接。")
	}
	return entry.botID, nil
}

// Load 返回工作空间。第一次读取时写入与桌面一致的预设猫咪。
func (s *Service) Load(ctx context.Context) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, err := s.ensureState(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Workspace: row.workspace, Pristine: row.pristine, Revision: row.revision}, nil
}

// Save 校验后整份替换工作空间。调用方需要先串行化自己的写入。
func (s *Service) Save(ctx context.Context, workspace Workspace) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateWorkspace(&workspace); err != nil {
		return Snapshot{}, err
	}
	row, err := s.ensureState(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	row.workspace = workspace
	row.pristine = false
	row.revision++
	if err = s.writeState(ctx, row); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Workspace: row.workspace, Pristine: false, Revision: row.revision}, nil
}

// LoadModelForm 返回表单和密钥是否存在，不返回密钥本身。
func (s *Service) LoadModelForm(ctx context.Context) (ModelFormView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, err := s.ensureState(ctx)
	if err != nil {
		return ModelFormView{}, err
	}
	return ModelFormView{ModelForm: row.settings, HasAPIKey: len(row.ciphertext) > 0}, nil
}

// SaveModelForm 保存表单。apiKey 为空表示保留原密钥；clearKey 为真时删除密钥。
func (s *Service) SaveModelForm(ctx context.Context, settings ModelForm, apiKey string, clearKey bool) (ModelFormView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateModelForm(&settings); err != nil {
		return ModelFormView{}, err
	}
	if utf16Len(apiKey) > 4096 {
		return ModelFormView{}, domain.NewError(400, "SETTINGS_INVALID", "API 密钥过长。")
	}
	row, err := s.ensureState(ctx)
	if err != nil {
		return ModelFormView{}, err
	}
	row.settings = settings
	// 模型表单和消息修订分开。保存设置不能让正在进行的发送被判成冲突。
	switch {
	case clearKey:
		row.nonce, row.ciphertext = nil, nil
	case apiKey != "":
		if len(s.key) != 32 {
			return ModelFormView{}, domain.NewError(500, "KEY_UNAVAILABLE", "服务端主密钥不可用，无法保存 API 密钥。")
		}
		ciphertext, nonce, sealErr := secure.Seal(s.key, []byte(apiKey), apiKeyRecord)
		if sealErr != nil {
			return ModelFormView{}, sealErr
		}
		row.ciphertext, row.nonce = ciphertext, nonce
	}
	if err = s.writeState(ctx, row); err != nil {
		return ModelFormView{}, err
	}
	s.logger.Info("已保存桌面模型设置", "provider", settings.Provider, "has_api_key", len(row.ciphertext) > 0)
	return ModelFormView{ModelForm: settings, HasAPIKey: len(row.ciphertext) > 0}, nil
}

// Dispatch 是交给原来的 agent 运行时的一次发送。模型调用不在这里发生。
type Dispatch struct {
	ConversationID string
	Content        string
	Attachments    []domain.AttachmentInput
	ReplyToID      string
	AgentID        string
	MessageID      string
	Revision       int64
	Done           bool
	Result         Delivery
}

// PrepareDispatch 校验消息并确保对应 Bot 与模型已存在。未配置模型时不改未发送状态。
func (s *Service) PrepareDispatch(ctx context.Context, agentID, messageID string) (Dispatch, error) {
	s.mu.Lock()
	row, err := s.ensureState(ctx)
	if err != nil {
		s.mu.Unlock()
		return Dispatch{}, err
	}
	agentIndex, messageIndex, findErr := findUserMessage(row.workspace, agentID, messageID)
	if findErr != nil {
		s.mu.Unlock()
		return Dispatch{}, findErr
	}
	message := row.workspace.Agents[agentIndex].Messages[messageIndex]
	if message.Status == "sent" {
		workspace := row.workspace
		s.mu.Unlock()
		return Dispatch{Done: true, Result: Delivery{Workspace: workspace}}, nil
	}
	if !modelReady(row.settings) {
		s.mu.Unlock()
		s.logger.Info("桌面消息未投递", "agent_id", agentID, "message_id", messageID, "result", "model_not_ready")
		return Dispatch{}, domain.NewError(409, "MODEL_NOT_READY", "尚未配置可用模型，请先在设置中填写 API 地址和模型名称。")
	}
	if messageCount(row.workspace) >= maxMessages {
		s.mu.Unlock()
		return Dispatch{}, domain.NewError(400, "WORKSPACE_INVALID", "消息已达 500 条上限，请先删除不需要的消息。")
	}
	agent := row.workspace.Agents[agentIndex]
	settings := row.settings
	revision := row.revision
	apiKey, keyErr := s.openAPIKey(row)
	s.mu.Unlock()
	if keyErr != nil {
		return Dispatch{}, keyErr
	}
	defer clearBytes(apiKey)
	modelID, err := s.modelForAgent(ctx, settings, string(apiKey), agent.ModelID)
	if err != nil {
		return Dispatch{}, err
	}
	conversationID, err := s.ensureBot(ctx, agent, modelID)
	if err != nil {
		return Dispatch{}, err
	}
	attachments := make([]domain.AttachmentInput, 0, len(message.Attachments))
	for _, item := range message.Attachments {
		attachments = append(attachments, domain.AttachmentInput{Path: item.Path, SHA256: item.SHA256})
	}
	replyToID := ""
	if message.ReplyTo != nil {
		replyToID = message.ReplyTo.MessageID
	}
	return Dispatch{ConversationID: conversationID, Content: message.Content, Attachments: attachments, ReplyToID: replyToID, AgentID: agentID, MessageID: messageID, Revision: revision}, nil
}

// MessageChrome 读取气泡的回复目标和表情。
func (s *Service) MessageChrome(ctx context.Context, messageID string) (string, []Reaction, error) {
	var reply string
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(reply_to_id,'') FROM messages WHERE id=?`, messageID).Scan(&reply)
	if err != nil {
		return "", nil, err
	}
	reactions, err := s.readReactions(ctx, messageID)
	return reply, reactions, err
}

// EnsureAgent 让群成员在发言前就有原来的 Bot 和模型。
func (s *Service) EnsureAgent(ctx context.Context, agentID string) error {
	s.mu.Lock()
	row, err := s.ensureState(ctx)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	var agent Agent
	found := false
	for _, item := range row.workspace.Agents {
		if item.ID == agentID {
			agent = item
			found = true
			break
		}
	}
	if !found {
		return domain.NewError(404, "AGENT_NOT_FOUND", "助手不存在")
	}
	modelID := ""
	if modelReady(row.settings) {
		apiKey, keyErr := s.openAPIKey(row)
		if keyErr != nil {
			return keyErr
		}
		defer clearBytes(apiKey)
		modelID, err = s.modelForAgent(ctx, row.settings, string(apiKey), agent.ModelID)
		if err != nil {
			return err
		}
	}
	_, err = s.ensureBot(ctx, agent, modelID)
	return err
}

// ApplyRun 把 agent 运行结果写回工作空间。失败时保留已生成的正文并标为 failed，方便重试。
func (s *Service) ApplyRun(ctx context.Context, agentID, messageID string, revision int64, reply string, runErr error) (Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.ensureState(ctx)
	if err != nil {
		return Delivery{}, err
	}
	if current.revision != revision {
		s.logger.Info("发送期间工作空间有新的保存，结果写回最新版本", "agent_id", agentID, "message_id", messageID, "captured_revision", revision, "current_revision", current.revision)
	}
	agentIndex, messageIndex, findErr := findUserMessage(current.workspace, agentID, messageID)
	if findErr != nil {
		return Delivery{}, findErr
	}
	messages := current.workspace.Agents[agentIndex].Messages
	target := messages[messageIndex]
	if target.RequestID == "" {
		target.RequestID = ids.New()
	}
	if twin, twinErr := s.findUserTwin(ctx, agentID, target.Content, target.ID); twinErr != nil {
		return Delivery{}, twinErr
	} else if twin != "" {
		target.ID = twin
		next := make([]ChatMessage, 0, len(messages))
		for index, message := range messages {
			if index == messageIndex {
				next = append(next, target)
				continue
			}
			if message.ID == twin {
				continue
			}
			next = append(next, message)
		}
		messages = next
		messageIndex = 0
		for index, message := range messages {
			if message.ID == target.ID {
				messageIndex = index
				break
			}
		}
	}
	current.workspace.Agents[agentIndex].Messages = messages
	current.workspace.Agents[agentIndex].Messages[messageIndex].RequestID = target.RequestID
	if runErr != nil && strings.TrimSpace(reply) == "" {
		current.workspace.Agents[agentIndex].Messages[messageIndex].Status = "failed"
		return s.storeRun(ctx, current, agentID, messageID, target.RequestID, "模型没有完成回复。", runErr)
	}
	status := "sent"
	code := ""
	notice := ""
	if runErr != nil {
		status = "failed"
		code = "MODEL_FAILED"
		notice = "模型没有完成回复。"
		var failure *domain.Error
		if errors.As(runErr, &failure) && failure.Message != "" {
			notice = failure.Message
		}
	}
	current.workspace.Agents[agentIndex].Messages[messageIndex].Status = status
	if strings.TrimSpace(reply) != "" {
		assistantID, findErr := s.findAssistantMessage(ctx, agentID, strings.TrimSpace(reply))
		if findErr != nil {
			return Delivery{}, findErr
		}
		if assistantID == "" {
			assistantID = ids.New()
		}
		replaced := false
		for index := range current.workspace.Agents[agentIndex].Messages {
			if current.workspace.Agents[agentIndex].Messages[index].ID != assistantID {
				continue
			}
			current.workspace.Agents[agentIndex].Messages[index].Content = strings.TrimSpace(reply)
			current.workspace.Agents[agentIndex].Messages[index].Status = status
			current.workspace.Agents[agentIndex].Messages[index].Role = "assistant"
			replaced = true
			break
		}
		if !replaced {
			current.workspace.Agents[agentIndex].Messages = append(current.workspace.Agents[agentIndex].Messages, ChatMessage{
				ID: assistantID, Role: "assistant", Content: strings.TrimSpace(reply), CreatedAt: s.now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
				Status: status, RequestID: target.RequestID, Reactions: []Reaction{},
			})
		}
	}
	if err = validateWorkspace(&current.workspace); err != nil {
		return Delivery{}, err
	}
	current.pristine = false
	current.revision++
	if err = s.writeState(ctx, current); err != nil {
		return Delivery{}, err
	}
	s.logger.Info("agent 运行结果已写入工作空间", "agent_id", agentID, "message_id", messageID, "request_id", target.RequestID, "status", status)
	return Delivery{Workspace: current.workspace, Code: code, Message: notice}, nil
}

func (s *Service) storeRun(ctx context.Context, current stateRow, agentID, messageID, requestID, notice string, runErr error) (Delivery, error) {
	var failure *domain.Error
	if errors.As(runErr, &failure) && failure.Message != "" {
		notice = failure.Message
	}
	current.pristine = false
	current.revision++
	if err := s.writeState(ctx, current); err != nil {
		return Delivery{}, err
	}
	s.logger.Warn("agent 运行失败", "agent_id", agentID, "message_id", messageID, "request_id", requestID)
	return Delivery{Workspace: current.workspace, Code: "MODEL_FAILED", Message: notice}, nil
}

func findUserMessage(workspace Workspace, agentID, messageID string) (int, int, error) {
	for agentIndex := range workspace.Agents {
		if workspace.Agents[agentIndex].ID != agentID {
			continue
		}
		for messageIndex := range workspace.Agents[agentIndex].Messages {
			message := workspace.Agents[agentIndex].Messages[messageIndex]
			if message.ID != messageID {
				continue
			}
			if message.Role != "user" {
				return 0, 0, domain.NewError(400, "MESSAGE_INVALID", "只能重新发送自己的消息。")
			}
			return agentIndex, messageIndex, nil
		}
		return 0, 0, domain.NewError(404, "MESSAGE_NOT_FOUND", "消息不存在")
	}
	return 0, 0, domain.NewError(404, "AGENT_NOT_FOUND", "助手不存在")
}

func (s *Service) openAPIKey(row stateRow) ([]byte, error) {
	if len(row.ciphertext) == 0 {
		return nil, nil
	}
	if len(s.key) != 32 {
		return nil, domain.NewError(500, "KEY_UNAVAILABLE", "服务端主密钥不可用。")
	}
	return secure.Open(s.key, row.ciphertext, row.nonce, apiKeyRecord)
}

func messageCount(workspace Workspace) int {
	total := 0
	for _, agent := range workspace.Agents {
		total += len(agent.Messages)
	}
	return total
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
