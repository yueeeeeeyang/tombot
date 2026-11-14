-- 将旧单模型版本原样转换为一个候选，保持历史配置、凭据和运行关联；新版本最多 16 个候选。
-- 此迁移扫描策略版本、运行和用量表并持有结构锁；生产需停止接单、备份后在维护窗口执行。
-- 失败整体回滚。降级必须同时恢复升级前数据库和程序，不修改历史脚本或迁移账本。
ALTER TABLE tombot.model_policy_versions ADD COLUMN routing_mode text NOT NULL DEFAULT 'priority'
    CHECK (routing_mode IN ('priority','weighted','round_robin'));
ALTER TABLE tombot.model_policy_versions ADD COLUMN creation_txid bigint NOT NULL DEFAULT 0;
ALTER TABLE tombot.model_policy_versions ALTER COLUMN creation_txid SET DEFAULT txid_current();
CREATE TABLE tombot.model_policy_candidates (
    policy_id text NOT NULL,
    version bigint NOT NULL,
    position integer NOT NULL CHECK (position BETWEEN 1 AND 16),
    provider_id text NOT NULL,
    model_id text NOT NULL,
    credential_id text,
    configuration jsonb NOT NULL CHECK (jsonb_typeof(configuration)='object' AND octet_length(configuration::text)<=16384),
    priority integer NOT NULL CHECK (priority BETWEEN 1 AND 1000),
    weight integer NOT NULL CHECK (weight BETWEEN 1 AND 1000),
    PRIMARY KEY(policy_id,version,model_id),
    UNIQUE(policy_id,version,position),
    FOREIGN KEY(policy_id,version) REFERENCES tombot.model_policy_versions(policy_id,version),
    FOREIGN KEY(provider_id,model_id) REFERENCES tombot.models(provider_id,id),
    FOREIGN KEY(provider_id,credential_id) REFERENCES tombot.model_credentials(provider_id,id)
);
INSERT INTO tombot.model_policy_candidates(policy_id,version,position,provider_id,model_id,credential_id,configuration,priority,weight)
    SELECT policy_id,version,1,provider_id,model_id,credential_id,configuration,1,1 FROM tombot.model_policy_versions;
CREATE INDEX model_policy_candidates_model ON tombot.model_policy_candidates(model_id);
CREATE INDEX model_policy_candidates_provider ON tombot.model_policy_candidates(provider_id);
CREATE INDEX model_policy_candidates_credential ON tombot.model_policy_candidates(credential_id) WHERE credential_id IS NOT NULL;
GRANT SELECT,INSERT ON tombot.model_policy_candidates TO tombot_control;
CREATE FUNCTION tombot.guard_model_candidate() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $guard$
BEGIN
    IF TG_OP<>'INSERT' OR NOT EXISTS(SELECT 1 FROM tombot.model_policy_versions
        WHERE policy_id=NEW.policy_id AND version=NEW.version AND status='draft' AND creation_txid=txid_current()) THEN
        RAISE EXCEPTION '候选只能随新草稿创建，历史候选不可改写' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$guard$;
CREATE TRIGGER model_policy_candidates_immutable BEFORE INSERT OR UPDATE OR DELETE ON tombot.model_policy_candidates
    FOR EACH ROW EXECUTE FUNCTION tombot.guard_model_candidate();
CREATE FUNCTION tombot.check_model_candidates() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $guard$
BEGIN
    IF NOT EXISTS(SELECT 1 FROM tombot.model_policy_candidates WHERE policy_id=NEW.policy_id AND version=NEW.version) THEN
        RAISE EXCEPTION '策略版本必须包含候选模型' USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END
$guard$;
CREATE CONSTRAINT TRIGGER model_policy_candidates_required AFTER INSERT ON tombot.model_policy_versions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION tombot.check_model_candidates();
CREATE TABLE tombot.model_route_counters (
    policy_id text NOT NULL,
    version bigint NOT NULL,
    next_ticket bigint NOT NULL DEFAULT 0 CHECK (next_ticket>=0),
    PRIMARY KEY(policy_id,version),
    FOREIGN KEY(policy_id,version) REFERENCES tombot.model_policy_versions(policy_id,version)
);
CREATE TABLE tombot.model_routes (
    user_id text NOT NULL,
    run_id text NOT NULL,
    policy_id text NOT NULL,
    version bigint NOT NULL,
    model_id text NOT NULL,
    attempted_models jsonb NOT NULL CHECK (jsonb_typeof(attempted_models)='array' AND jsonb_array_length(attempted_models) BETWEEN 1 AND 16 AND attempted_models->>-1=model_id),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(user_id,run_id),
    FOREIGN KEY(user_id,run_id,policy_id,version) REFERENCES tombot.runs(user_id,id,model_policy_id,model_version),
    FOREIGN KEY(policy_id,version,model_id) REFERENCES tombot.model_policy_candidates(policy_id,version,model_id)
);
INSERT INTO tombot.model_routes(user_id,run_id,policy_id,version,model_id,attempted_models)
    SELECT r.user_id,r.id,r.model_policy_id,r.model_version,v.model_id,jsonb_build_array(v.model_id)
    FROM tombot.runs r JOIN tombot.model_policy_versions v ON v.policy_id=r.model_policy_id AND v.version=r.model_version;
GRANT SELECT,INSERT,UPDATE ON tombot.model_route_counters,tombot.model_routes TO tombot_control;
ALTER TABLE tombot.model_routes ENABLE ROW LEVEL SECURITY;
ALTER TABLE tombot.model_routes FORCE ROW LEVEL SECURITY;
CREATE POLICY model_routes_control ON tombot.model_routes TO tombot_control USING(true) WITH CHECK(true);
CREATE POLICY model_routes_tenant ON tombot.model_routes FOR SELECT TO tombot_tenant
    USING(user_id=nullif(current_setting('tombot.user_id',true),''));
GRANT SELECT ON tombot.model_routes TO tombot_tenant;
CREATE FUNCTION tombot.guard_model_route() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $guard$
BEGIN
    IF (NEW.user_id,NEW.run_id,NEW.policy_id,NEW.version) IS DISTINCT FROM (OLD.user_id,OLD.run_id,OLD.policy_id,OLD.version)
       OR OLD.attempted_models ? NEW.model_id
       OR NEW.attempted_models IS DISTINCT FROM OLD.attempted_models || jsonb_build_array(NEW.model_id) THEN
        RAISE EXCEPTION '运行路由只能追加未尝试过的候选' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$guard$;
CREATE TRIGGER model_routes_fixed BEFORE UPDATE ON tombot.model_routes FOR EACH ROW EXECUTE FUNCTION tombot.guard_model_route();
ALTER TABLE tombot.usage_reservations ADD COLUMN model_id text;
DROP TRIGGER usage_reservations_fixed ON tombot.usage_reservations;
UPDATE tombot.usage_reservations u SET model_id=v.model_id FROM tombot.model_policy_versions v
    WHERE v.policy_id=u.policy_id AND v.version=u.model_version;
CREATE TRIGGER usage_reservations_fixed BEFORE UPDATE ON tombot.usage_reservations
    FOR EACH ROW EXECUTE FUNCTION tombot.protect_usage_reservation();
ALTER TABLE tombot.usage_reservations ALTER COLUMN model_id SET NOT NULL;
ALTER TABLE tombot.usage_reservations ADD CONSTRAINT usage_reservations_candidate
    FOREIGN KEY(policy_id,model_version,model_id) REFERENCES tombot.model_policy_candidates(policy_id,version,model_id);
CREATE OR REPLACE FUNCTION tombot.guard_model_catalog() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, tombot, pg_temp AS $guard$
BEGIN
    IF TG_OP = 'DELETE' THEN
        -- 只有停用后的目录才允许删除；被任何策略版本引用时整体拒绝，保证历史配置可追溯。
        IF OLD.is_enabled THEN
            RAISE EXCEPTION '模型目录必须停用后才能删除' USING ERRCODE = '23514';
        END IF;
        IF TG_TABLE_NAME = 'model_providers' THEN
            IF EXISTS (SELECT 1 FROM tombot.model_policy_candidates WHERE provider_id = OLD.id) THEN
                RAISE EXCEPTION '仍被策略版本引用的供应商不能删除' USING ERRCODE = '23514';
            END IF;
        ELSE
            IF EXISTS (SELECT 1 FROM tombot.model_policy_candidates WHERE model_id = OLD.id) THEN
                RAISE EXCEPTION '仍被策略版本引用的模型不能删除' USING ERRCODE = '23514';
            END IF;
        END IF;
        RETURN OLD;
    END IF;
    IF TG_TABLE_NAME = 'model_providers' AND NOT OLD.is_enabled AND NOT NEW.is_enabled THEN
        IF (to_jsonb(NEW) - ARRAY['name','protocol','base_url','revision']) IS DISTINCT FROM
           (to_jsonb(OLD) - ARRAY['name','protocol','base_url','revision']) OR NEW.revision <> OLD.revision + 1 THEN
            RAISE EXCEPTION '停用供应商仅允许编辑名称、协议和地址并递增修订号' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_TABLE_NAME = 'models' AND NOT OLD.is_enabled AND NOT NEW.is_enabled THEN
        IF (to_jsonb(NEW) - ARRAY['model_key','display_name','context_limit','output_limit','supports_tools','supports_vision','reasoning_effort','metadata','revision']) IS DISTINCT FROM
           (to_jsonb(OLD) - ARRAY['model_key','display_name','context_limit','output_limit','supports_tools','supports_vision','reasoning_effort','metadata','revision']) OR NEW.revision <> OLD.revision + 1 THEN
            RAISE EXCEPTION '停用模型仅允许编辑参数并递增修订号' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) - ARRAY['is_enabled', 'revision']) IS DISTINCT FROM
       (to_jsonb(OLD) - ARRAY['is_enabled', 'revision']) OR NEW.revision <> OLD.revision + 1 THEN
        RAISE EXCEPTION '模型目录内容不可原地修改' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$guard$;

REVOKE ALL ON FUNCTION tombot.guard_model_catalog() FROM PUBLIC;

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
        IF EXISTS (SELECT 1 FROM tombot.model_policy_candidates c JOIN tombot.model_policy_versions v USING(policy_id,version) WHERE c.credential_id=OLD.id AND v.status<>'revoked') THEN
            RAISE EXCEPTION '仍被策略引用的凭据不能销毁' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION '凭据只允许首次撤销、符合条件的销毁或随停用供应商删除' USING ERRCODE = '23514';
END
$guard$;

CREATE OR REPLACE FUNCTION tombot.check_usage_binding() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,tombot,pg_temp AS $function$
DECLARE configuration jsonb;
BEGIN
    IF NEW.user_id IS DISTINCT FROM nullif(current_setting('tombot.user_id',true),'') THEN
        RAISE EXCEPTION '用量所属用户无效' USING ERRCODE='23514';
    END IF;
    SELECT v.configuration INTO configuration FROM tombot.model_policy_candidates v
        WHERE v.policy_id=NEW.policy_id AND v.version=NEW.model_version AND v.model_id=NEW.model_id;
    IF NOT EXISTS(SELECT 1 FROM tombot.model_routes r WHERE r.user_id=NEW.user_id AND r.run_id=NEW.run_id AND r.model_id=NEW.model_id) OR configuration IS NULL
       OR NEW.reserved_tokens IS DISTINCT FROM ((configuration->>'context_limit')::bigint+(configuration->>'output_limit')::bigint)
       OR NEW.prices IS DISTINCT FROM configuration->'prices'
       OR NEW.input_includes_cache IS DISTINCT FROM (configuration->>'protocol' IN ('openai_compat','responses')) THEN
        RAISE EXCEPTION '用量预留与固定模型版本不符' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.check_usage_binding() FROM PUBLIC;
CREATE OR REPLACE FUNCTION tombot.guard_model_policy() RETURNS trigger
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
    IF NEW.current_version IS NULL AND OLD.current_version IS NOT NULL THEN
        SELECT status INTO version_status FROM tombot.model_policy_versions WHERE policy_id=OLD.id AND version=OLD.current_version;
        IF version_status IS DISTINCT FROM 'revoked' THEN
            RAISE EXCEPTION '当前版本仅在撤销时清除激活状态' USING ERRCODE='23514';
        END IF;
    END IF;
    IF NEW.current_version IS DISTINCT FROM OLD.current_version AND NEW.current_version IS NOT NULL THEN
        SELECT status INTO version_status FROM tombot.model_policy_versions
            WHERE policy_id = NEW.id AND version = NEW.current_version FOR SHARE;
        IF version_status IS DISTINCT FROM 'published' THEN
            RAISE EXCEPTION '当前策略版本尚未发布' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END
$guard$;
ALTER TABLE tombot.model_policy_versions DROP COLUMN provider_id, DROP COLUMN model_id, DROP COLUMN credential_id, DROP COLUMN configuration;
REVOKE ALL ON FUNCTION tombot.guard_model_candidate(),tombot.check_model_candidates(),tombot.guard_model_route(),tombot.guard_model_policy(),tombot.guard_model_credential() FROM PUBLIC;
