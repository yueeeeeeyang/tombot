-- 消息、回合和运行直接属于登录用户，调度连接只能访问不含正文的队列。
ALTER TABLE tombot.conversations ADD COLUMN turn_sequence bigint NOT NULL DEFAULT 0 CHECK (turn_sequence>=0);

CREATE TABLE tombot.turns (
    user_id text NOT NULL,
    id text NOT NULL CHECK (id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    conversation_id text NOT NULL,
    sequence bigint NOT NULL CHECK (sequence>0),
    client_request_id text NOT NULL CHECK (client_request_id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    request_digest bytea NOT NULL CHECK (octet_length(request_digest)=32),
    source text NOT NULL CHECK (source IN ('chat','group','bot_dm','proactive','routine')),
    status text NOT NULL CHECK (status IN ('queued','running','canceling','succeeded','failed','canceled','unknown')),
    error_code text NOT NULL DEFAULT '' CHECK (error_code='' OR error_code ~ '^[A-Z][A-Z0-9_]{0,63}$'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    finished_at timestamptz,
    PRIMARY KEY (user_id,id),
    UNIQUE (user_id,id,conversation_id),
    UNIQUE (user_id,conversation_id,sequence),
    UNIQUE (user_id,client_request_id),
    FOREIGN KEY (user_id,conversation_id) REFERENCES tombot.conversations(user_id,id),
    CHECK ((status IN ('queued','running','canceling'))=(finished_at IS NULL))
);
CREATE UNIQUE INDEX turns_active_conversation ON tombot.turns(user_id,conversation_id) WHERE status IN ('queued','running','canceling');

CREATE TABLE tombot.runs (
    user_id text NOT NULL,
    id text NOT NULL CHECK (id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    turn_id text NOT NULL,
    conversation_id text NOT NULL,
    bot_id text NOT NULL,
    bot_revision bigint NOT NULL CHECK (bot_revision>0),
    bot_snapshot jsonb NOT NULL CHECK (jsonb_typeof(bot_snapshot)='object' AND octet_length(bot_snapshot::text)<=131072),
    model_policy_id text,
    model_version bigint,
    status text NOT NULL CHECK (status IN ('queued','running','canceling','succeeded','failed','canceled','unknown')),
    error_code text NOT NULL DEFAULT '' CHECK (error_code='' OR error_code ~ '^[A-Z][A-Z0-9_]{0,63}$'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    started_at timestamptz,
    finished_at timestamptz,
    PRIMARY KEY (user_id,id),
    UNIQUE (user_id,id,bot_id),
    UNIQUE (user_id,id,turn_id,conversation_id),
    UNIQUE (user_id,id,bot_id,conversation_id,turn_id),
    FOREIGN KEY (user_id,turn_id,conversation_id) REFERENCES tombot.turns(user_id,id,conversation_id),
    FOREIGN KEY (user_id,bot_id) REFERENCES tombot.bots(user_id,id),
    FOREIGN KEY (model_policy_id,model_version) REFERENCES tombot.model_policy_versions(policy_id,version),
    CHECK ((model_policy_id IS NULL AND model_version IS NULL) OR
        (model_policy_id IS NOT NULL AND model_version IS NOT NULL AND model_version>0)),
    CHECK ((status IN ('queued','running','canceling'))=(finished_at IS NULL))
);
CREATE INDEX runs_turn ON tombot.runs(user_id,turn_id,id);
CREATE INDEX runs_page ON tombot.runs(user_id,id DESC);

CREATE FUNCTION tombot.protect_run_binding() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF (NEW.user_id,NEW.id,NEW.turn_id,NEW.conversation_id,NEW.bot_id,NEW.bot_revision,NEW.bot_snapshot)
        IS DISTINCT FROM (OLD.user_id,OLD.id,OLD.turn_id,OLD.conversation_id,OLD.bot_id,OLD.bot_revision,OLD.bot_snapshot)
        OR (OLD.model_policy_id IS NOT NULL AND
            (NEW.model_policy_id,NEW.model_version) IS DISTINCT FROM (OLD.model_policy_id,OLD.model_version)) THEN
        RAISE EXCEPTION '运行身份与已绑定配置不可修改' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.protect_run_binding() FROM PUBLIC;
CREATE TRIGGER runs_fixed_binding BEFORE UPDATE ON tombot.runs FOR EACH ROW EXECUTE FUNCTION tombot.protect_run_binding();

CREATE TABLE tombot.messages (
    user_id text NOT NULL,
    id text NOT NULL CHECK (id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    conversation_id text NOT NULL,
    turn_id text NOT NULL,
    run_id text,
    sequence bigint NOT NULL CHECK (sequence>0),
    sender_kind text NOT NULL CHECK (sender_kind IN ('user','bot','system')),
    sender_bot_id text,
    blocks jsonb NOT NULL CHECK (jsonb_typeof(blocks)='array' AND jsonb_array_length(blocks) BETWEEN 1 AND 128 AND octet_length(blocks::text)<=1048576),
    status text NOT NULL CHECK (status IN ('complete','interrupted')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id,id),
    UNIQUE (user_id,conversation_id,sequence),
    FOREIGN KEY (user_id,turn_id,conversation_id) REFERENCES tombot.turns(user_id,id,conversation_id),
    FOREIGN KEY (user_id,run_id,turn_id,conversation_id) REFERENCES tombot.runs(user_id,id,turn_id,conversation_id),
    FOREIGN KEY (user_id,run_id,sender_bot_id) REFERENCES tombot.runs(user_id,id,bot_id),
    CHECK ((sender_kind='bot' AND sender_bot_id IS NOT NULL AND run_id IS NOT NULL) OR
        (sender_kind IN ('user','system') AND sender_bot_id IS NULL))
);
CREATE INDEX messages_turn ON tombot.messages(user_id,turn_id,sequence);

CREATE TABLE tombot.user_dispatch_heads (
    user_id text PRIMARY KEY REFERENCES tombot.users(id),
    last_claimed_at timestamptz
);
CREATE INDEX user_dispatch_fairness ON tombot.user_dispatch_heads(last_claimed_at NULLS FIRST,user_id);

CREATE TABLE tombot.jobs (
    user_id text NOT NULL,
    id text NOT NULL CHECK (id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    run_id text NOT NULL,
    bot_id text NOT NULL,
    conversation_id text NOT NULL,
    turn_id text NOT NULL,
    status text NOT NULL CHECK (status IN ('queued','leased','finished')),
    available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    lease_owner text,
    lease_expires_at timestamptz,
    fencing_token bigint NOT NULL DEFAULT 0 CHECK (fencing_token>=0),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt>=0),
    cancel_requested_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id,id),
    UNIQUE (user_id,run_id),
    FOREIGN KEY (user_id,run_id,bot_id,conversation_id,turn_id) REFERENCES tombot.runs(user_id,id,bot_id,conversation_id,turn_id),
    CHECK ((status='leased' AND lease_owner IS NOT NULL AND lease_owner ~ '^[0-9A-HJKMNP-TV-Z]{26}$' AND lease_expires_at IS NOT NULL AND attempt>0) OR
        (status<>'leased' AND lease_owner IS NULL AND lease_expires_at IS NULL))
);
CREATE UNIQUE INDEX jobs_active_bot ON tombot.jobs(user_id,bot_id) WHERE status='leased';
CREATE INDEX jobs_queue ON tombot.jobs(user_id,available_at,created_at,id) WHERE status='queued';
CREATE INDEX jobs_expired ON tombot.jobs(lease_expires_at,user_id,id) WHERE status='leased';

CREATE TABLE tombot.run_operations (
    user_id text NOT NULL,
    id text NOT NULL CHECK (id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    run_id text NOT NULL,
    step_key text NOT NULL CHECK (step_key ~ '^[a-z][a-z0-9_.:-]{0,119}$'),
    kind text NOT NULL CHECK (kind IN ('model','tool','computer')),
    request_digest bytea NOT NULL CHECK (octet_length(request_digest)=32),
    fencing_token bigint NOT NULL CHECK (fencing_token>0),
    attempt integer NOT NULL CHECK (attempt>0),
    status text NOT NULL CHECK (status IN ('started','succeeded','failed','unknown')),
    result jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(result)='object' AND octet_length(result::text)<=65536),
    error_code text NOT NULL DEFAULT '' CHECK (error_code='' OR error_code ~ '^[A-Z][A-Z0-9_]{0,63}$'),
    started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    finished_at timestamptz,
    PRIMARY KEY (user_id,id),
    UNIQUE (user_id,run_id,step_key),
    FOREIGN KEY (user_id,run_id) REFERENCES tombot.runs(user_id,id),
    CHECK ((status='started')=(finished_at IS NULL))
);
CREATE UNIQUE INDEX run_operations_inflight ON tombot.run_operations(user_id,run_id) WHERE status='started';

CREATE FUNCTION tombot.protect_run_operation() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF (NEW.user_id,NEW.id,NEW.run_id,NEW.step_key,NEW.kind,NEW.request_digest,NEW.fencing_token,NEW.attempt,NEW.started_at)
        IS DISTINCT FROM (OLD.user_id,OLD.id,OLD.run_id,OLD.step_key,OLD.kind,OLD.request_digest,OLD.fencing_token,OLD.attempt,OLD.started_at)
        OR (OLD.status<>'started' AND NEW IS DISTINCT FROM OLD) THEN
        RAISE EXCEPTION '操作身份和终态不可改写' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.protect_run_operation() FROM PUBLIC;
CREATE TRIGGER run_operations_fixed BEFORE UPDATE ON tombot.run_operations FOR EACH ROW EXECUTE FUNCTION tombot.protect_run_operation();

DO $policies$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['turns','runs','messages','jobs','user_dispatch_heads','run_operations'] LOOP
        EXECUTE format('ALTER TABLE tombot.%I ENABLE ROW LEVEL SECURITY',table_name);
        EXECUTE format('ALTER TABLE tombot.%I FORCE ROW LEVEL SECURITY',table_name);
        EXECUTE format('CREATE POLICY tenant_scope ON tombot.%I TO tombot_tenant USING (user_id=nullif(current_setting(''tombot.user_id'',true),'''')) WITH CHECK (user_id=nullif(current_setting(''tombot.user_id'',true),''''))',table_name);
    END LOOP;
END
$policies$;
GRANT SELECT,INSERT,UPDATE ON tombot.turns,tombot.runs,tombot.jobs,tombot.user_dispatch_heads,tombot.run_operations TO tombot_tenant;
GRANT SELECT,INSERT ON tombot.messages TO tombot_tenant;
CREATE POLICY dispatch_jobs ON tombot.jobs TO tombot_dispatch USING (true) WITH CHECK (true);
CREATE POLICY dispatch_heads ON tombot.user_dispatch_heads TO tombot_dispatch USING (true) WITH CHECK (true);
GRANT SELECT ON tombot.jobs,tombot.user_dispatch_heads TO tombot_dispatch;
GRANT UPDATE(status,lease_owner,lease_expires_at,fencing_token,attempt,updated_at) ON tombot.jobs TO tombot_dispatch;
GRANT UPDATE(last_claimed_at) ON tombot.user_dispatch_heads TO tombot_dispatch;
