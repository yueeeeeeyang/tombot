-- 扩展协议约束并统一 Responses 缓存计量；不修改既有目录或历史策略快照。
-- 约束校验会短暂锁定并扫描供应商目录，规模随供应商数量增长；迁移事务失败整体回滚。
-- 回滚使用升级前数据库备份，或新增前向修复脚本；禁止将新协议记录交给旧二进制。
ALTER TABLE tombot.model_providers DROP CONSTRAINT model_providers_protocol_check;
ALTER TABLE tombot.model_providers ADD CONSTRAINT model_providers_protocol_check
    CHECK (protocol IN ('openai_compat', 'anthropic', 'responses'));

CREATE OR REPLACE FUNCTION tombot.check_usage_binding() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,tombot,pg_temp AS $function$
DECLARE configuration jsonb;
BEGIN
    IF NEW.user_id IS DISTINCT FROM nullif(current_setting('tombot.user_id',true),'') THEN
        RAISE EXCEPTION '用量所属用户无效' USING ERRCODE='23514';
    END IF;
    SELECT v.configuration INTO configuration FROM tombot.model_policy_versions v
        WHERE v.policy_id=NEW.policy_id AND v.version=NEW.model_version;
    IF configuration IS NULL
       OR NEW.reserved_tokens IS DISTINCT FROM ((configuration->>'context_limit')::bigint+(configuration->>'output_limit')::bigint)
       OR NEW.prices IS DISTINCT FROM configuration->'prices'
       OR NEW.input_includes_cache IS DISTINCT FROM (configuration->>'protocol' IN ('openai_compat','responses')) THEN
        RAISE EXCEPTION '用量预留与固定模型版本不符' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.check_usage_binding() FROM PUBLIC;
