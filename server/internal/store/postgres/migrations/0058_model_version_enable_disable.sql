-- 版本仅保留启用/停用语义，以策略 current_version 为唯一启用来源，避免版本状态与指针冲突。
-- 首次启用仍需能力验证；旧已发布版本保留验证时间，旧撤销及草稿版本转为未验证的停用版本。
-- 扫描配置表并短暂持结构锁；维护窗口先备份，失败事务回滚，降级须整体恢复数据库与程序。
DROP TRIGGER model_policy_versions_immutable ON tombot.model_policy_versions;
ALTER TABLE tombot.model_policy_versions RENAME COLUMN published_at TO verified_at;
UPDATE tombot.model_policy_versions SET verified_at=NULL WHERE status<>'published';

CREATE OR REPLACE FUNCTION tombot.guard_model_policy() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $guard$
BEGIN
    IF TG_OP='INSERT' THEN
        IF NEW.current_version IS NOT NULL THEN
            RAISE EXCEPTION '新策略尚无已验证版本' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.id<>OLD.id OR NEW.name<>OLD.name OR NEW.created_at<>OLD.created_at OR NEW.revision<>OLD.revision+1 THEN
        RAISE EXCEPTION '策略标识或修订号无效' USING ERRCODE='23514';
    END IF;
    IF NEW.current_version IS NOT NULL AND NOT EXISTS(SELECT 1 FROM tombot.model_policy_versions
        WHERE policy_id=NEW.id AND version=NEW.current_version AND verified_at IS NOT NULL) THEN
        RAISE EXCEPTION '策略版本必须先通过能力验证才能启用' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$guard$;
UPDATE tombot.model_policies p SET current_version=NULL,revision=revision+1
    WHERE current_version IS NOT NULL AND NOT EXISTS(SELECT 1 FROM tombot.model_policy_versions v
        WHERE v.policy_id=p.id AND v.version=p.current_version AND v.verified_at IS NOT NULL);
ALTER TABLE tombot.model_policy_versions DROP COLUMN status;

CREATE OR REPLACE FUNCTION tombot.guard_model_policy_version() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $guard$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION '策略版本不能删除' USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' THEN
        IF NEW.verified_at IS NOT NULL OR NEW.creation_txid<>txid_current() THEN
            RAISE EXCEPTION '新策略版本必须停用且尚未验证' USING ERRCODE='23514';
        END IF;
    ELSE
        IF (to_jsonb(NEW)-'verified_at') IS DISTINCT FROM (to_jsonb(OLD)-'verified_at')
           OR OLD.verified_at IS NOT NULL OR NEW.verified_at IS NULL THEN
            RAISE EXCEPTION '策略配置不可改写，仅允许首次记录验证时间' USING ERRCODE='23514';
        END IF;
        NEW.verified_at:=clock_timestamp();
    END IF;
    RETURN NEW;
END
$guard$;
CREATE TRIGGER model_policy_versions_immutable BEFORE INSERT OR UPDATE OR DELETE ON tombot.model_policy_versions
    FOR EACH ROW EXECUTE FUNCTION tombot.guard_model_policy_version();

CREATE OR REPLACE FUNCTION tombot.guard_model_candidate() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $guard$
BEGIN
    IF TG_OP<>'INSERT' OR NOT EXISTS(SELECT 1 FROM tombot.model_policy_versions
        WHERE policy_id=NEW.policy_id AND version=NEW.version AND verified_at IS NULL AND creation_txid=txid_current()) THEN
        RAISE EXCEPTION '候选只能随新版本创建，历史候选不可改写' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$guard$;

CREATE OR REPLACE FUNCTION tombot.guard_model_credential() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, tombot, pg_temp AS $guard$
BEGIN
    IF TG_OP = 'DELETE' THEN
        -- 凭据只能随停用的供应商一起删除，且不能仍被策略版本引用。
        IF EXISTS (SELECT 1 FROM tombot.model_policy_candidates WHERE credential_id = OLD.id) THEN
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
        IF EXISTS (SELECT 1 FROM tombot.model_policy_candidates c JOIN tombot.model_policies p ON p.id=c.policy_id AND p.current_version=c.version WHERE c.credential_id=OLD.id) THEN
            RAISE EXCEPTION '仍被策略引用的凭据不能销毁' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION '凭据只允许首次撤销、符合条件的销毁或随停用供应商删除' USING ERRCODE = '23514';
END
$guard$;

REVOKE ALL ON FUNCTION tombot.guard_model_policy(),tombot.guard_model_policy_version(),tombot.guard_model_candidate(),tombot.guard_model_credential() FROM PUBLIC;
