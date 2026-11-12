-- 额度与操作在同一租户事务预留；未知用量保留占用，不以零用量释放。
ALTER TABLE tombot.run_operations ADD CONSTRAINT run_operations_metering_identity UNIQUE(user_id,id,run_id);

CREATE TABLE tombot.usage_accounts (
    user_id text PRIMARY KEY REFERENCES tombot.users(id)
);
CREATE TABLE tombot.usage_periods (
    user_id text NOT NULL REFERENCES tombot.users(id),
    period date NOT NULL CHECK (extract(day FROM period)=1),
    known_tokens bigint NOT NULL DEFAULT 0 CHECK (known_tokens>=0),
    held_tokens bigint NOT NULL DEFAULT 0 CHECK (held_tokens>=0),
    PRIMARY KEY(user_id,period)
);
CREATE TABLE tombot.usage_reservations (
    user_id text NOT NULL,
    id text NOT NULL,
    run_id text NOT NULL,
    bot_id text NOT NULL,
    policy_id text NOT NULL,
    model_version bigint NOT NULL,
    period date NOT NULL,
    fencing_token bigint NOT NULL CHECK(fencing_token>0),
    attempt integer NOT NULL CHECK(attempt>0),
    reserved_tokens bigint NOT NULL CHECK(reserved_tokens BETWEEN 1 AND 200000000),
    held_tokens bigint NOT NULL CHECK(held_tokens>=0),
    prices jsonb NOT NULL CHECK(jsonb_typeof(prices)='object' AND octet_length(prices::text)<=2048),
    input_includes_cache boolean NOT NULL,
    state text NOT NULL CHECK(state IN ('active','unknown','settled')),
    slot_expires_at timestamptz NOT NULL,
    slot_released boolean NOT NULL DEFAULT false,
    needs_reconcile boolean NOT NULL DEFAULT false,
    actual_tokens bigint CHECK(actual_tokens>=0),
    cost_micro_usd bigint CHECK(cost_micro_usd>=0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(user_id,id),
    FOREIGN KEY(user_id,id,run_id) REFERENCES tombot.run_operations(user_id,id,run_id),
    FOREIGN KEY(user_id,run_id,bot_id) REFERENCES tombot.runs(user_id,id,bot_id),
    FOREIGN KEY(policy_id,model_version) REFERENCES tombot.model_policy_versions(policy_id,version),
    FOREIGN KEY(user_id,period) REFERENCES tombot.usage_periods(user_id,period),
    CHECK ((state='active')=(NOT slot_released)),
    CHECK ((state='settled' AND held_tokens=0 AND actual_tokens IS NOT NULL) OR
        (state<>'settled' AND held_tokens>=reserved_tokens AND actual_tokens IS NULL AND cost_micro_usd IS NULL))
);
CREATE INDEX usage_reservations_active ON tombot.usage_reservations(user_id,id) WHERE NOT slot_released;
CREATE INDEX usage_reservations_expired ON tombot.usage_reservations(slot_expires_at,user_id,id) WHERE state='active';
CREATE INDEX usage_reservations_reconcile ON tombot.usage_reservations(user_id,id) WHERE needs_reconcile;
CREATE INDEX usage_reservations_period ON tombot.usage_reservations(user_id,period,id);

-- 每条证据不可修改，迟到结果只能追加；不会取得运行写入权。
CREATE TABLE tombot.model_usage (
    user_id text NOT NULL,
    id text NOT NULL CHECK(id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    reservation_id text NOT NULL,
    source text NOT NULL CHECK(source IN ('worker','timeout')),
    reported jsonb CHECK(jsonb_typeof(reported)='object' AND octet_length(reported::text)<=4096),
    is_final boolean NOT NULL,
    measured_tokens bigint CHECK(measured_tokens>=0),
    lower_bound_tokens bigint NOT NULL CHECK(lower_bound_tokens>=0),
    cost_micro_usd bigint CHECK(cost_micro_usd>=0),
    evidence_digest bytea NOT NULL CHECK(octet_length(evidence_digest)=32),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(user_id,id),
    FOREIGN KEY(user_id,reservation_id) REFERENCES tombot.usage_reservations(user_id,id),
    CHECK(measured_tokens IS NULL OR measured_tokens>=lower_bound_tokens),
    CHECK(cost_micro_usd IS NULL OR measured_tokens IS NOT NULL)
);
CREATE INDEX model_usage_reservation ON tombot.model_usage(user_id,reservation_id,created_at,id);

CREATE FUNCTION tombot.protect_usage_reservation() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF (to_jsonb(NEW)-ARRAY['held_tokens','state','slot_released','needs_reconcile','actual_tokens','cost_micro_usd','updated_at'])
        IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['held_tokens','state','slot_released','needs_reconcile','actual_tokens','cost_micro_usd','updated_at'])
        OR (OLD.state<>'active' AND NEW.state='active')
        OR (OLD.state='settled' AND (NEW.state,NEW.held_tokens,NEW.actual_tokens,NEW.cost_micro_usd)
            IS DISTINCT FROM (OLD.state,OLD.held_tokens,OLD.actual_tokens,OLD.cost_micro_usd)) THEN
        RAISE EXCEPTION '用量预留身份和已结算金额不可改写' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.protect_usage_reservation() FROM PUBLIC;
CREATE TRIGGER usage_reservations_fixed BEFORE UPDATE ON tombot.usage_reservations
    FOR EACH ROW EXECUTE FUNCTION tombot.protect_usage_reservation();

DO $policies$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['usage_accounts','usage_periods','usage_reservations','model_usage'] LOOP
        EXECUTE format('ALTER TABLE tombot.%I ENABLE ROW LEVEL SECURITY',table_name);
        EXECUTE format('ALTER TABLE tombot.%I FORCE ROW LEVEL SECURITY',table_name);
        EXECUTE format('CREATE POLICY tenant_scope ON tombot.%I TO tombot_tenant USING (user_id=nullif(current_setting(''tombot.user_id'',true),'''')) WITH CHECK (user_id=nullif(current_setting(''tombot.user_id'',true),''''))',table_name);
    END LOOP;
END
$policies$;
GRANT SELECT,INSERT,UPDATE ON tombot.usage_accounts,tombot.usage_periods,tombot.usage_reservations TO tombot_tenant;
GRANT SELECT,INSERT ON tombot.model_usage TO tombot_tenant;
CREATE POLICY dispatch_usage ON tombot.usage_reservations FOR SELECT TO tombot_dispatch USING(true);
GRANT SELECT(user_id,id,state,slot_expires_at,needs_reconcile) ON tombot.usage_reservations TO tombot_dispatch;
CREATE POLICY control_usage ON tombot.usage_reservations FOR SELECT TO tombot_control USING(true);
CREATE POLICY control_usage_periods ON tombot.usage_periods FOR SELECT TO tombot_control USING(true);
GRANT SELECT ON tombot.usage_reservations,tombot.usage_periods TO tombot_control;
