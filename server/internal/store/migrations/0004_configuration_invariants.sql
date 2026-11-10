-- 默认模型全库唯一；在事务内先清除旧默认，再设置新默认。
CREATE UNIQUE INDEX idx_models_default ON models(is_default) WHERE is_default=1;
CREATE INDEX idx_bots_updated ON bots(updated_at DESC,id DESC);
CREATE INDEX idx_conversations_cursor ON conversations(updated_at DESC,id DESC);
-- 密钥引用通过触发器约束，避免修改已经发布的初始迁移。
CREATE TRIGGER providers_secret_insert BEFORE INSERT ON providers
WHEN NEW.secret_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM secrets WHERE id=NEW.secret_id)
BEGIN SELECT RAISE(ABORT,'secret_not_found'); END;
CREATE TRIGGER providers_secret_update BEFORE UPDATE OF secret_id ON providers
WHEN NEW.secret_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM secrets WHERE id=NEW.secret_id)
BEGIN SELECT RAISE(ABORT,'secret_not_found'); END;
CREATE TRIGGER secrets_provider_restrict BEFORE DELETE ON secrets
WHEN EXISTS(SELECT 1 FROM providers WHERE secret_id=OLD.id)
BEGIN SELECT RAISE(ABORT,'secret_in_use'); END;
