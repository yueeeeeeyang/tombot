-- 新模型配置仅前向发布；历史引用不能随目录编辑或凭据替换而改变。
ALTER TABLE tombot.model_policies ADD COLUMN revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0);
ALTER TABLE tombot.model_policies ADD COLUMN current_version bigint;
ALTER TABLE tombot.model_policies ADD CONSTRAINT model_policies_current_version
    FOREIGN KEY (id, current_version) REFERENCES tombot.model_policy_versions(policy_id, version);
ALTER TABLE tombot.models ADD COLUMN revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0);
ALTER TABLE tombot.model_credentials ADD CONSTRAINT model_credentials_envelope
    CHECK (octet_length(nonce) = 12 AND octet_length(ciphertext) BETWEEN 17 AND 8208
        AND length(key_reference) BETWEEN 1 AND 120);

CREATE FUNCTION tombot.guard_model_catalog() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, tombot, pg_temp AS $guard$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION '模型目录必须停用，不能删除历史引用' USING ERRCODE = '23514';
    END IF;
    IF (to_jsonb(NEW) - ARRAY['is_enabled', 'revision']) IS DISTINCT FROM
       (to_jsonb(OLD) - ARRAY['is_enabled', 'revision']) OR NEW.revision <> OLD.revision + 1 THEN
        RAISE EXCEPTION '模型目录内容不可原地修改' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$guard$;
CREATE TRIGGER model_providers_immutable BEFORE UPDATE OR DELETE ON tombot.model_providers
    FOR EACH ROW EXECUTE FUNCTION tombot.guard_model_catalog();
CREATE TRIGGER models_immutable BEFORE UPDATE OR DELETE ON tombot.models
    FOR EACH ROW EXECUTE FUNCTION tombot.guard_model_catalog();

CREATE FUNCTION tombot.guard_model_credential() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, tombot, pg_temp AS $guard$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION '凭据须先撤销并通过专用退役流程清除' USING ERRCODE = '23514';
    END IF;
    IF (to_jsonb(NEW) - 'revoked_at') IS DISTINCT FROM (to_jsonb(OLD) - 'revoked_at')
       OR OLD.revoked_at IS NOT NULL OR NEW.revoked_at IS NULL THEN
        RAISE EXCEPTION '凭据版本只允许首次撤销' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$guard$;
CREATE TRIGGER model_credentials_immutable BEFORE UPDATE OR DELETE ON tombot.model_credentials
    FOR EACH ROW EXECUTE FUNCTION tombot.guard_model_credential();

CREATE FUNCTION tombot.guard_model_policy_version() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, tombot, pg_temp AS $guard$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION '策略版本不能删除' USING ERRCODE = '23514';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'draft' OR NEW.published_at IS NOT NULL THEN
            RAISE EXCEPTION '新策略版本必须从草稿开始' USING ERRCODE = '23514';
        END IF;
    ELSE
        IF (to_jsonb(NEW) - ARRAY['status', 'published_at']) IS DISTINCT FROM
           (to_jsonb(OLD) - ARRAY['status', 'published_at'])
           OR NOT ((OLD.status = 'draft' AND NEW.status IN ('published', 'revoked'))
               OR (OLD.status = 'published' AND NEW.status = 'revoked')) THEN
            RAISE EXCEPTION '策略配置不可改写或恢复已撤销版本' USING ERRCODE = '23514';
        END IF;
        IF NEW.status = 'published' THEN
            NEW.published_at := transaction_timestamp();
        ELSIF NEW.published_at IS DISTINCT FROM OLD.published_at THEN
            RAISE EXCEPTION '策略发布时间不可改写' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END
$guard$;
CREATE TRIGGER model_policy_versions_immutable BEFORE INSERT OR UPDATE OR DELETE ON tombot.model_policy_versions
    FOR EACH ROW EXECUTE FUNCTION tombot.guard_model_policy_version();

CREATE FUNCTION tombot.guard_model_policy() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, tombot, pg_temp AS $guard$
DECLARE version_status text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.current_version IS NOT NULL THEN
            RAISE EXCEPTION '新策略尚无已发布版本' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.id <> OLD.id OR NEW.name <> OLD.name OR NEW.created_at <> OLD.created_at
        OR NEW.revision <> OLD.revision + 1 THEN
        RAISE EXCEPTION '策略标识或修订号无效' USING ERRCODE = '23514';
    END IF;
    IF NEW.current_version IS DISTINCT FROM OLD.current_version THEN
        IF NEW.current_version IS NULL OR NEW.current_version <= coalesce(OLD.current_version, 0) THEN
            RAISE EXCEPTION '策略只能发布更高版本' USING ERRCODE = '23514';
        END IF;
        SELECT status INTO version_status FROM tombot.model_policy_versions
            WHERE policy_id = NEW.id AND version = NEW.current_version FOR SHARE;
        IF version_status IS DISTINCT FROM 'published' THEN
            RAISE EXCEPTION '当前策略版本尚未发布' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END
$guard$;
CREATE TRIGGER model_policies_revision BEFORE INSERT OR UPDATE ON tombot.model_policies
    FOR EACH ROW EXECUTE FUNCTION tombot.guard_model_policy();

REVOKE DELETE ON tombot.model_providers, tombot.models, tombot.model_credentials,
    tombot.model_policies, tombot.model_policy_versions FROM tombot_control;
REVOKE ALL ON FUNCTION tombot.guard_model_catalog(), tombot.guard_model_credential(),
    tombot.guard_model_policy_version(), tombot.guard_model_policy() FROM PUBLIC;

-- 空库及当前小型配置表在有界迁移事务内升级；新增约束会扫描凭据表并取得结构锁。
-- 失败整体回滚，不改写既有配置。发布后若需修正约束，只能新增更高版本前向脚本。
