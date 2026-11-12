-- 计量输入必须与运行固定版本相符，防止执行器误传低额度或其他版本价格。
ALTER TABLE tombot.runs ADD CONSTRAINT runs_metering_binding UNIQUE(user_id,id,model_policy_id,model_version);
ALTER TABLE tombot.run_operations ADD CONSTRAINT run_operations_metering_attempt UNIQUE(user_id,id,run_id,fencing_token,attempt);
ALTER TABLE tombot.usage_reservations ADD CONSTRAINT usage_reservations_run_binding
    FOREIGN KEY(user_id,run_id,policy_id,model_version) REFERENCES tombot.runs(user_id,id,model_policy_id,model_version);
ALTER TABLE tombot.usage_reservations ADD CONSTRAINT usage_reservations_operation_attempt
    FOREIGN KEY(user_id,id,run_id,fencing_token,attempt) REFERENCES tombot.run_operations(user_id,id,run_id,fencing_token,attempt);
CREATE FUNCTION tombot.check_usage_binding() RETURNS trigger
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
       OR NEW.input_includes_cache IS DISTINCT FROM (configuration->>'protocol'='openai_compat') THEN
        RAISE EXCEPTION '用量预留与固定模型版本不符' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.check_usage_binding() FROM PUBLIC;
CREATE TRIGGER usage_reservations_binding BEFORE INSERT ON tombot.usage_reservations
    FOR EACH ROW EXECUTE FUNCTION tombot.check_usage_binding();

-- 已知 token 不改写；后续完整证据可将未知费用补齐，但不能改动已知费用。
CREATE OR REPLACE FUNCTION tombot.protect_usage_reservation() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF (to_jsonb(NEW)-ARRAY['held_tokens','state','slot_released','needs_reconcile','actual_tokens','cost_micro_usd','updated_at'])
        IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['held_tokens','state','slot_released','needs_reconcile','actual_tokens','cost_micro_usd','updated_at'])
        OR (OLD.state<>'active' AND NEW.state='active')
        OR (OLD.state='settled' AND ((NEW.state,NEW.held_tokens,NEW.actual_tokens)
            IS DISTINCT FROM (OLD.state,OLD.held_tokens,OLD.actual_tokens)
            OR OLD.cost_micro_usd IS NOT NULL AND NEW.cost_micro_usd IS DISTINCT FROM OLD.cost_micro_usd)) THEN
        RAISE EXCEPTION '用量预留身份和已结算金额不可改写' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.protect_usage_reservation() FROM PUBLIC;
CREATE POLICY control_model_usage ON tombot.model_usage FOR SELECT TO tombot_control USING(true);
GRANT SELECT ON tombot.model_usage TO tombot_control;
