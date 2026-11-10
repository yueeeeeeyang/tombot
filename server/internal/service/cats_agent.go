package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"io.yueyang/tom/internal/domain"
)

// syncModel 把桌面里填写的供应商写入原来的目录和密钥，返回模型标识。
func (s *Service) syncModel(ctx context.Context, form ModelForm, apiKey string) (string, error) {
	if s.catalog == nil || s.secrets == nil {
		return "", domain.NewError(500, "AGENT_UNAVAILABLE", "服务端 agent 还没有启动。")
	}
	kind := "openai_compat"
	if form.Provider == "anthropic" {
		kind = "anthropic"
	}
	secretID, err := s.ensureSecret(ctx, apiKey)
	if err != nil {
		return "", err
	}
	providerID, err := s.ensureProvider(ctx, kind, form.BaseURL, secretID)
	if err != nil {
		return "", err
	}
	tokens, _ := parseTokenLimit(form.MaxOutputTokens)
	if tokens < 1 {
		tokens = 1024
	}
	contextWindow := 8192
	if tokens > contextWindow {
		contextWindow = tokens
	}
	return s.ensureModel(ctx, providerID, form.ModelName, contextWindow, tokens)
}

// modelForAgent 先记下设置里的供应商，再让这只猫咪的 modelId 成为实际调用的模型。
func (s *Service) modelForAgent(ctx context.Context, form ModelForm, apiKey, catalogID string) (string, error) {
	if _, err := s.syncModel(ctx, form, apiKey); err != nil {
		return "", err
	}
	key := strings.TrimSpace(catalogID)
	if key == "" {
		key = form.ModelName
	}
	var providerID string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM providers WHERE display_name='桌面' ORDER BY created_at LIMIT 1`).Scan(&providerID); err != nil {
		return "", err
	}
	tokens, _ := parseTokenLimit(form.MaxOutputTokens)
	if tokens < 1 {
		tokens = 1024
	}
	contextWindow := 8192
	if tokens > contextWindow {
		contextWindow = tokens
	}
	return s.ensureModel(ctx, providerID, key, contextWindow, tokens)
}

func (s *Service) ensureSecret(ctx context.Context, apiKey string) (*string, error) {
	if apiKey == "" {
		var id string
		err := s.db.QueryRowContext(ctx, `SELECT id FROM secrets WHERE name='桌面模型'`).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return &id, err
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM secrets WHERE name='桌面模型'`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		meta, createErr := s.secrets.Create(ctx, "桌面模型", apiKey, "desktop")
		if createErr != nil {
			return nil, createErr
		}
		return &meta.ID, nil
	}
	if err != nil {
		return nil, err
	}
	if err = s.secrets.Rotate(ctx, id, apiKey, "desktop"); err != nil {
		return nil, err
	}
	return &id, nil
}

func (s *Service) ensureProvider(ctx context.Context, kind, baseURL string, secretID *string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM providers WHERE display_name='桌面' ORDER BY created_at LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		created, createErr := s.catalog.CreateProvider(ctx, ProviderInput{Kind: kind, DisplayName: "桌面", BaseURL: baseURL, SecretID: secretID}, "desktop")
		return created.ID, createErr
	}
	if err != nil {
		return "", err
	}
	patch := ProviderPatch{Kind: Field[string]{Set: true, Value: kind}, BaseURL: Field[string]{Set: true, Value: baseURL}, IsEnabled: Field[bool]{Set: true, Value: true}}
	if secretID != nil {
		patch.SecretID = Field[*string]{Set: true, Value: secretID}
	}
	updated, err := s.catalog.UpdateProvider(ctx, id, patch, "desktop")
	return updated.ID, err
}

func (s *Service) ensureModel(ctx context.Context, providerID, modelKey string, contextWindow, maxOutput int) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM models WHERE provider_id=? AND model_key=?`, providerID, modelKey).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		supports := true
		created, createErr := s.catalog.CreateModel(ctx, ModelInput{
			ProviderID: providerID, ModelKey: modelKey, DisplayName: modelKey,
			ContextWindow: contextWindow, MaxOutput: maxOutput, SupportsTools: &supports, SupportsVision: &supports, IsDefault: true,
		}, "desktop")
		return created.ID, createErr
	}
	if err != nil {
		return "", err
	}
	updated, err := s.catalog.UpdateModel(ctx, id, ModelPatch{MaxOutput: Field[int]{Set: true, Value: maxOutput}, ContextWindow: Field[int]{Set: true, Value: contextWindow}}, "desktop")
	return updated.ID, err
}

// ensureBot 保证这只猫咪有原来的 Bot 和工作区。身份按助手标识复用，不另建一套执行对象。
func (s *Service) ensureBot(ctx context.Context, agent Agent, modelID string) (string, error) {
	if s.bots == nil {
		return "", domain.NewError(500, "AGENT_UNAVAILABLE", "服务端 agent 还没有启动。")
	}
	var botID, conversationID string
	err := s.db.QueryRowContext(ctx, `SELECT bots.id, conversations.id FROM bots JOIN conversations ON conversations.bot_id=bots.id AND conversations.kind='direct' AND conversations.source!='routine' WHERE bots.id=? OR bots.desktop_id=?`, agent.ID, agent.ID).Scan(&botID, &conversationID)
	if err == nil {
		patch := BotPatch{Name: Field[string]{Set: true, Value: agent.Name}, Soul: Field[string]{Set: true, Value: desktopSoul}, ToolPolicy: Field[domain.ToolPolicy]{Set: true, Value: desktopToolPolicy()}}
		if modelID != "" {
			patch.ModelID = Field[*string]{Set: true, Value: &modelID}
		}
		_, updateErr := s.bots.Update(ctx, botID, patch, "desktop")
		return conversationID, updateErr
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	role := fitRole(agent.Description)
	var modelPtr *string
	if modelID != "" {
		modelPtr = &modelID
	}
	policy := desktopToolPolicy()
	created, err := s.bots.Create(ctx, BotInput{
		ID: agent.ID, Name: agent.Name, RoleTitle: role,
		Soul:       desktopSoul,
		ModelID:    modelPtr,
		ToolPolicy: &policy,
	}, "desktop")
	if err != nil {
		return "", err
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE bots SET desktop_id=?, group_name=?, description=?, avatar_preset=?, catalog_model_id=?, draft=?, computer_url=? WHERE id=? AND desktop_id IS NULL`, agent.ID, agent.Group, agent.Description, agent.Avatar, agent.ModelID, agent.Draft, agent.ComputerURL, created.ID); err != nil {
		return "", err
	}
	return created.ConversationID, nil
}

func fitRole(description string) string {
	if description == "" {
		return "助手"
	}
	runes := []rune(description)
	if len(runes) > 40 {
		description = string(runes[:40])
	}
	for len(description) > 300 && len([]rune(description)) > 1 {
		runes = []rune(description)
		description = string(runes[:len(runes)-1])
	}
	return description
}

// BotID 返回猫咪对应的 Bot，电脑页要用它的工作区。
func (s *Service) BotID(ctx context.Context, agentID string) (string, error) {
	var botID string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM bots WHERE id=? OR desktop_id=?`, agentID, agentID).Scan(&botID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", domain.NewError(404, "AGENT_NOT_FOUND", "这只猫咪还没有电脑。")
	}
	return botID, err
}
