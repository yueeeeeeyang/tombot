-- 停用的供应商可以连同其模型与凭据一并删除；仍然引用历史策略版本的配置必须保留。
-- 删除会连带清理目录行，审计记录仍保留删除动作与被删标识。

CREATE OR REPLACE FUNCTION tombot.guard_model_catalog() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, tombot, pg_temp AS $guard$
BEGIN
    IF TG_OP = 'DELETE' THEN
        -- 只有停用后的目录才允许删除；被任何策略版本引用时整体拒绝，保证历史配置可追溯。
        IF OLD.is_enabled THEN
            RAISE EXCEPTION '模型目录必须停用后才能删除' USING ERRCODE = '23514';
        END IF;
        IF TG_TABLE_NAME = 'model_providers' THEN
            IF EXISTS (SELECT 1 FROM tombot.model_policy_versions WHERE provider_id = OLD.id) THEN
                RAISE EXCEPTION '仍被策略版本引用的供应商不能删除' USING ERRCODE = '23514';
            END IF;
        ELSE
            IF EXISTS (SELECT 1 FROM tombot.model_policy_versions WHERE model_id = OLD.id) THEN
                RAISE EXCEPTION '仍被策略版本引用的模型不能删除' USING ERRCODE = '23514';
            END IF;
        END IF;
        RETURN OLD;
    END IF;
    IF (to_jsonb(NEW) - ARRAY['is_enabled', 'revision']) IS DISTINCT FROM
       (to_jsonb(OLD) - ARRAY['is_enabled', 'revision']) OR NEW.revision <> OLD.revision + 1 THEN
        RAISE EXCEPTION '模型目录内容不可原地修改' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$guard$;

CREATE OR REPLACE FUNCTION tombot.guard_model_credential() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, tombot, pg_temp AS $guard$
BEGIN
    IF TG_OP = 'DELETE' THEN
        -- 凭据只能随停用的供应商一起删除，且不能仍被策略版本引用。
        IF EXISTS (SELECT 1 FROM tombot.model_policy_versions WHERE credential_id = OLD.id) THEN
            RAISE EXCEPTION '仍被策略版本引用的凭据不能删除' USING ERRCODE = '23514';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM tombot.model_providers WHERE id = OLD.provider_id AND NOT is_enabled) THEN
            RAISE EXCEPTION '凭据只能随停用的供应商删除' USING ERRCODE = '23514';
        END IF;
        RETURN OLD;
    END IF;
    IF (to_jsonb(NEW) - ARRAY['revoked_at','destroyed_at','nonce','ciphertext']) IS DISTINCT FROM
       (to_jsonb(OLD) - ARRAY['revoked_at','destroyed_at','nonce','ciphertext']) THEN
        RAISE EXCEPTION '凭据版本身份不可修改' USING ERRCODE = '23514';
    END IF;
    IF OLD.revoked_at IS NULL AND NEW.revoked_at IS NOT NULL AND NEW.destroyed_at IS NULL
       AND NEW.nonce IS NOT DISTINCT FROM OLD.nonce AND NEW.ciphertext IS NOT DISTINCT FROM OLD.ciphertext THEN
        RETURN NEW;
    END IF;
    IF OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS NOT DISTINCT FROM OLD.revoked_at
       AND OLD.destroyed_at IS NULL AND NEW.destroyed_at IS NOT NULL AND NEW.nonce IS NULL AND NEW.ciphertext IS NULL THEN
        IF EXISTS (SELECT 1 FROM tombot.model_policy_versions WHERE credential_id=OLD.id AND status<>'revoked') THEN
            RAISE EXCEPTION '仍被策略引用的凭据不能销毁' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION '凭据只允许首次撤销、符合条件的销毁或随停用供应商删除' USING ERRCODE = '23514';
END
$guard$;

REVOKE ALL ON FUNCTION tombot.guard_model_catalog(), tombot.guard_model_credential() FROM PUBLIC;

-- 删除权限只授予控制连接，判断条件仍由上面的触发器承担。
GRANT DELETE ON tombot.model_credentials, tombot.models, tombot.model_providers TO tombot_control;
