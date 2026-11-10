package desktop

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"io.yueyang/tom/internal/auth"
	"io.yueyang/tom/internal/domain"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store"
)

const apiKeyRecord = "desktop-settings"

// Service 保存桌面工作空间，并在模型已配置时投递消息。密钥加密后存放，接口不回传明文。
type Service struct {
	mu       sync.Mutex
	db       *sql.DB
	auth     *auth.Service
	key      []byte
	complete Completer
	logger   *slog.Logger
	now      func() time.Time
}

// Dependencies 是桌面服务的显式依赖。Completer 为空时使用带超时的 HTTP 实现。
type Dependencies struct {
	DB        *sql.DB
	Auth      *auth.Service
	Key       []byte
	Logger    *slog.Logger
	Completer Completer
	Now       func() time.Time
}

// Snapshot 是一次读取到的工作空间。Pristine 为真时才接受本机旧档导入。
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
	settings   Settings
	nonce      []byte
	ciphertext []byte
}

// New 组装桌面服务。数据库和认证缺失时直接失败，不用空实现假装可用。
func New(dep Dependencies) (*Service, error) {
	if dep.DB == nil || dep.Auth == nil {
		return nil, errors.New("桌面服务缺少数据库或认证")
	}
	logger := dep.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	completer := dep.Completer
	if completer == nil {
		completer = NewHTTPCompleter()
	}
	now := dep.Now
	if now == nil {
		now = time.Now
	}
	return &Service{db: dep.DB, auth: dep.Auth, key: dep.Key, complete: completer, logger: logger, now: now}, nil
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

// Import 只在服务端还是预设、没有用户改动时接收本机旧档。之后拒绝覆盖。
func (s *Service) Import(ctx context.Context, workspace Workspace) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateWorkspace(&workspace); err != nil {
		return Snapshot{}, err
	}
	row, err := s.ensureState(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if !row.pristine {
		return Snapshot{}, domain.NewError(409, "WORKSPACE_NOT_PRISTINE", "服务端已有工作空间，不能再用本机旧档覆盖。")
	}
	row.workspace = workspace
	row.pristine = false
	row.revision++
	if err = s.writeState(ctx, row); err != nil {
		return Snapshot{}, err
	}
	s.logger.Info("已导入桌面旧工作空间", "revision", row.revision)
	return Snapshot{Workspace: workspace, Pristine: false, Revision: row.revision}, nil
}

// LoadSettings 返回表单和密钥是否存在，不返回密钥本身。
func (s *Service) LoadSettings(ctx context.Context) (SettingsView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, err := s.ensureState(ctx)
	if err != nil {
		return SettingsView{}, err
	}
	return SettingsView{Settings: row.settings, HasAPIKey: len(row.ciphertext) > 0}, nil
}

// SaveSettings 保存表单。apiKey 为空表示保留原密钥；clearKey 为真时删除密钥。
func (s *Service) SaveSettings(ctx context.Context, settings Settings, apiKey string, clearKey bool) (SettingsView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateSettings(&settings); err != nil {
		return SettingsView{}, err
	}
	if utf16Len(apiKey) > 4096 {
		return SettingsView{}, domain.NewError(400, "SETTINGS_INVALID", "API 密钥过长。")
	}
	row, err := s.ensureState(ctx)
	if err != nil {
		return SettingsView{}, err
	}
	row.settings = settings
	switch {
	case clearKey:
		row.nonce, row.ciphertext = nil, nil
	case apiKey != "":
		if len(s.key) != 32 {
			return SettingsView{}, domain.NewError(500, "KEY_UNAVAILABLE", "服务端主密钥不可用，无法保存 API 密钥。")
		}
		ciphertext, nonce, sealErr := secure.Seal(s.key, []byte(apiKey), apiKeyRecord)
		if sealErr != nil {
			return SettingsView{}, sealErr
		}
		row.ciphertext, row.nonce = ciphertext, nonce
	}
	row.revision++
	if err = s.writeState(ctx, row); err != nil {
		return SettingsView{}, err
	}
	s.logger.Info("已保存桌面模型设置", "provider", settings.Provider, "has_api_key", len(row.ciphertext) > 0)
	return SettingsView{Settings: settings, HasAPIKey: len(row.ciphertext) > 0}, nil
}

// Deliver 把一条用户消息交给已配置的模型。未配置时保持 unsent，不把草稿伪装成已发送。
func (s *Service) Deliver(ctx context.Context, agentID, messageID string) (Delivery, error) {
	s.mu.Lock()
	row, err := s.ensureState(ctx)
	if err != nil {
		s.mu.Unlock()
		return Delivery{}, err
	}
	agentIndex, messageIndex, findErr := findUserMessage(row.workspace, agentID, messageID)
	if findErr != nil {
		s.mu.Unlock()
		return Delivery{}, findErr
	}
	message := row.workspace.Agents[agentIndex].Messages[messageIndex]
	if message.Status == "sent" {
		workspace := row.workspace
		s.mu.Unlock()
		return Delivery{Workspace: workspace}, nil
	}
	if !modelReady(row.settings) {
		s.mu.Unlock()
		s.logger.Info("桌面消息未投递", "agent_id", agentID, "message_id", messageID, "result", "model_not_ready")
		return Delivery{}, domain.NewError(409, "MODEL_NOT_READY", "尚未配置可用模型，请先在设置中填写 API 地址和模型名称。")
	}
	if messageCount(row.workspace) >= maxMessages {
		s.mu.Unlock()
		return Delivery{}, domain.NewError(400, "WORKSPACE_INVALID", "消息已达 500 条上限，请先删除不需要的消息。")
	}
	settings := row.settings
	revision := row.revision
	messages := append([]ChatMessage(nil), row.workspace.Agents[agentIndex].Messages...)
	apiKey, keyErr := s.openAPIKey(row)
	s.mu.Unlock()
	if keyErr != nil {
		return Delivery{}, keyErr
	}
	defer clearBytes(apiKey)

	tokens, _ := parseTokenLimit(settings.MaxOutputTokens)
	reply, callErr := s.complete.Complete(ctx, Completion{
		BaseURL: settings.BaseURL, APIKey: string(apiKey), Model: settings.ModelName,
		Temperature: settings.Temperature, TopP: settings.TopP, MaxTokens: tokens,
		Messages: transcript(messages),
	})

	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.ensureState(ctx)
	if err != nil {
		return Delivery{}, err
	}
	if current.revision != revision {
		return Delivery{}, domain.NewError(409, "WORKSPACE_CONFLICT", "工作空间刚被更新，请重新发送。")
	}
	agentIndex, messageIndex, findErr = findUserMessage(current.workspace, agentID, messageID)
	if findErr != nil {
		return Delivery{}, findErr
	}
	target := &current.workspace.Agents[agentIndex].Messages[messageIndex]
	if callErr != nil {
		target.Status = "failed"
		if target.RequestID == "" {
			target.RequestID = ids.New()
		}
		current.pristine = false
		current.revision++
		if err = s.writeState(ctx, current); err != nil {
			return Delivery{}, err
		}
		s.logger.Warn("桌面消息投递失败", "agent_id", agentID, "message_id", messageID)
		notice := "模型没有完成回复。"
		var failure *domain.Error
		if errors.As(callErr, &failure) && failure.Message != "" {
			notice = failure.Message
		}
		return Delivery{Workspace: current.workspace, Code: "MODEL_FAILED", Message: notice}, nil
	}
	target.Status = "sent"
	if target.RequestID == "" {
		target.RequestID = ids.New()
	}
	current.workspace.Agents[agentIndex].Messages = append(current.workspace.Agents[agentIndex].Messages, ChatMessage{
		ID: ids.New(), Role: "assistant", Content: reply, CreatedAt: s.now().UTC().Format(time.RFC3339Nano),
		Status: "sent", RequestID: target.RequestID, Reactions: []Reaction{},
	})
	if err = validateWorkspace(&current.workspace); err != nil {
		return Delivery{}, err
	}
	current.pristine = false
	current.revision++
	if err = s.writeState(ctx, current); err != nil {
		return Delivery{}, err
	}
	s.logger.Info("桌面消息已投递", "agent_id", agentID, "message_id", messageID, "request_id", target.RequestID)
	return Delivery{Workspace: current.workspace}, nil
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

func (s *Service) ensureState(ctx context.Context) (stateRow, error) {
	row, err := s.readState(ctx)
	if err == nil {
		return row, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return stateRow{}, err
	}
	workspace := SeedWorkspace()
	settings := DefaultSettings()
	encodedWorkspace, err := json.Marshal(workspace)
	if err != nil {
		return stateRow{}, err
	}
	encodedSettings, err := json.Marshal(settings)
	if err != nil {
		return stateRow{}, err
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		_, execErr := tx.ExecContext(ctx, `INSERT INTO desktop_state(id,workspace_json,revision,pristine,settings_json,updated_at) VALUES(1,?,1,1,?,?)`, string(encodedWorkspace), string(encodedSettings), s.now().UnixMilli())
		return execErr
	})
	if err != nil {
		// 并发首次读取时另一事务可能已经插入。
		if row, readErr := s.readState(ctx); readErr == nil {
			return row, nil
		}
		return stateRow{}, err
	}
	return stateRow{workspace: workspace, revision: 1, pristine: true, settings: settings}, nil
}

func (s *Service) readState(ctx context.Context) (stateRow, error) {
	var workspaceJSON, settingsJSON string
	var revision int64
	var pristine int
	var nonce, ciphertext []byte
	err := s.db.QueryRowContext(ctx, `SELECT workspace_json,revision,pristine,settings_json,api_key_nonce,api_key_ciphertext FROM desktop_state WHERE id=1`).Scan(&workspaceJSON, &revision, &pristine, &settingsJSON, &nonce, &ciphertext)
	if err != nil {
		return stateRow{}, err
	}
	workspace, err := ParseWorkspace([]byte(workspaceJSON))
	if err != nil {
		return stateRow{}, domain.NewError(500, "WORKSPACE_CORRUPT", "服务端工作空间已损坏，原始内容已保留。")
	}
	settings, err := ParseSettings([]byte(settingsJSON))
	if err != nil {
		return stateRow{}, domain.NewError(500, "SETTINGS_CORRUPT", "服务端模型设置已损坏，原始内容已保留。")
	}
	return stateRow{workspace: workspace, revision: revision, pristine: pristine == 1, settings: settings, nonce: nonce, ciphertext: ciphertext}, nil
}

func (s *Service) writeState(ctx context.Context, row stateRow) error {
	encodedWorkspace, err := json.Marshal(row.workspace)
	if err != nil {
		return err
	}
	encodedSettings, err := json.Marshal(row.settings)
	if err != nil {
		return err
	}
	pristine := 0
	if row.pristine {
		pristine = 1
	}
	return store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		result, execErr := tx.ExecContext(ctx, `UPDATE desktop_state SET workspace_json=?,revision=?,pristine=?,settings_json=?,api_key_nonce=?,api_key_ciphertext=?,updated_at=? WHERE id=1`, string(encodedWorkspace), row.revision, pristine, string(encodedSettings), nullableBytes(row.nonce), nullableBytes(row.ciphertext), s.now().UnixMilli())
		if execErr != nil {
			return execErr
		}
		affected, execErr := result.RowsAffected()
		if execErr != nil {
			return execErr
		}
		if affected != 1 {
			return domain.NewError(500, "WORKSPACE_MISSING", "桌面工作空间不存在。")
		}
		return nil
	})
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
