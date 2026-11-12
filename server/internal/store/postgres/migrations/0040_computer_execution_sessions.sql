-- 电脑执行权独立于任务租约。清理确认前保留独占，超时本身不能释放电脑。
ALTER TABLE tombot.jobs ADD CONSTRAINT jobs_computer_binding UNIQUE(user_id,id,run_id,bot_id);

CREATE TABLE tombot.computer_execution_heads (
    user_id text PRIMARY KEY,
    computer_id text NOT NULL,
    last_ticket bigint NOT NULL DEFAULT 0 CHECK(last_ticket>=0),
    last_fencing_token bigint NOT NULL DEFAULT 0 CHECK(last_fencing_token>=0),
    FOREIGN KEY(user_id,computer_id) REFERENCES tombot.computers(user_id,id)
);

CREATE TABLE tombot.computer_sessions (
    user_id text NOT NULL,
    id text NOT NULL CHECK(id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    computer_id text NOT NULL,
    node_id text NOT NULL REFERENCES tombot.computer_nodes(id),
    container_id text NOT NULL CHECK(container_id ~ '^[0-9a-f]{64}$'),
    generation bigint NOT NULL CHECK(generation>0),
    job_id text NOT NULL,
    run_id text NOT NULL,
    bot_id text NOT NULL,
    job_owner text NOT NULL CHECK(job_owner ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    job_fencing_token bigint NOT NULL CHECK(job_fencing_token>0),
    job_attempt integer NOT NULL CHECK(job_attempt>0),
    ticket bigint NOT NULL CHECK(ticket>0),
    status text NOT NULL CHECK(status IN ('waiting','starting','active','draining','quarantined','released')),
    fencing_token bigint,
    revision bigint NOT NULL DEFAULT 0 CHECK(revision>=0),
    wait_deadline_at timestamptz NOT NULL,
    hold_deadline_at timestamptz,
    lease_expires_at timestamptz,
    error_code text NOT NULL DEFAULT '' CHECK(error_code='' OR error_code ~ '^[A-Z][A-Z0-9_]{0,63}$'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_checked_at timestamptz,
    next_check_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    cleanup_attempt integer NOT NULL DEFAULT 0 CHECK(cleanup_attempt>=0),
    released_at timestamptz,
    PRIMARY KEY(user_id,id),
    UNIQUE(user_id,ticket),
    UNIQUE(user_id,fencing_token),
    UNIQUE(user_id,run_id,job_fencing_token,job_attempt),
    UNIQUE(user_id,id,run_id,job_fencing_token,job_attempt),
    FOREIGN KEY(user_id,computer_id) REFERENCES tombot.computers(user_id,id),
    FOREIGN KEY(user_id,job_id,run_id,bot_id) REFERENCES tombot.jobs(user_id,id,run_id,bot_id),
    CHECK((status='released')=(released_at IS NOT NULL)),
    CHECK((fencing_token IS NULL AND revision=0 AND hold_deadline_at IS NULL AND lease_expires_at IS NULL AND status IN ('waiting','released')) OR
          (fencing_token IS NOT NULL AND fencing_token>0 AND revision>0 AND hold_deadline_at IS NOT NULL AND lease_expires_at IS NOT NULL AND status<>'waiting'))
);
ALTER TABLE tombot.run_operations ADD COLUMN computer_session_id text;
ALTER TABLE tombot.run_operations ADD CONSTRAINT run_operations_computer_kind CHECK((kind='computer')=(computer_session_id IS NOT NULL));
ALTER TABLE tombot.run_operations ADD CONSTRAINT run_operations_computer_owner FOREIGN KEY(user_id,computer_session_id,run_id,fencing_token,attempt)
    REFERENCES tombot.computer_sessions(user_id,id,run_id,job_fencing_token,job_attempt);

CREATE FUNCTION tombot.protect_operation_computer() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF NEW.computer_session_id IS DISTINCT FROM OLD.computer_session_id THEN
        RAISE EXCEPTION '操作绑定电脑会话不可修改' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.protect_operation_computer() FROM PUBLIC;
CREATE TRIGGER run_operations_computer_fixed BEFORE UPDATE ON tombot.run_operations FOR EACH ROW EXECUTE FUNCTION tombot.protect_operation_computer();
CREATE UNIQUE INDEX computer_sessions_exclusive ON tombot.computer_sessions(user_id,computer_id)
    WHERE status IN ('starting','active','draining','quarantined');
CREATE INDEX computer_sessions_queue ON tombot.computer_sessions(user_id,ticket) WHERE status='waiting';
CREATE INDEX computer_sessions_reconcile ON tombot.computer_sessions(last_checked_at NULLS FIRST,user_id,id) WHERE status<>'released';

CREATE FUNCTION tombot.protect_computer_session() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF (NEW.user_id,NEW.id,NEW.computer_id,NEW.node_id,NEW.container_id,NEW.generation,NEW.job_id,NEW.run_id,NEW.bot_id,
        NEW.job_owner,NEW.job_fencing_token,NEW.job_attempt,NEW.ticket,NEW.wait_deadline_at,NEW.created_at)
       IS DISTINCT FROM
       (OLD.user_id,OLD.id,OLD.computer_id,OLD.node_id,OLD.container_id,OLD.generation,OLD.job_id,OLD.run_id,OLD.bot_id,
        OLD.job_owner,OLD.job_fencing_token,OLD.job_attempt,OLD.ticket,OLD.wait_deadline_at,OLD.created_at)
       OR (OLD.fencing_token IS NOT NULL AND (NEW.fencing_token,NEW.hold_deadline_at) IS DISTINCT FROM (OLD.fencing_token,OLD.hold_deadline_at))
       OR NEW.revision<OLD.revision
       OR (OLD.status='released' AND NEW IS DISTINCT FROM OLD)
       OR (NEW.status<>OLD.status AND NOT (
           (OLD.status='waiting' AND NEW.status IN ('starting','released')) OR
           (OLD.status='starting' AND NEW.status IN ('active','draining','quarantined')) OR
           (OLD.status='active' AND NEW.status IN ('draining','quarantined')) OR
           (OLD.status IN ('draining','quarantined') AND NEW.status IN ('draining','quarantined','released')))) THEN
        RAISE EXCEPTION '电脑会话身份或状态不可改写' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.protect_computer_session() FROM PUBLIC;
CREATE TRIGGER computer_sessions_fixed BEFORE UPDATE ON tombot.computer_sessions FOR EACH ROW EXECUTE FUNCTION tombot.protect_computer_session();

DO $policies$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['computer_execution_heads','computer_sessions'] LOOP
        EXECUTE format('ALTER TABLE tombot.%I ENABLE ROW LEVEL SECURITY',table_name);
        EXECUTE format('ALTER TABLE tombot.%I FORCE ROW LEVEL SECURITY',table_name);
        EXECUTE format('CREATE POLICY tenant_scope ON tombot.%I TO tombot_tenant USING (user_id=nullif(current_setting(''tombot.user_id'',true),'''')) WITH CHECK (user_id=nullif(current_setting(''tombot.user_id'',true),''''))',table_name);
    END LOOP;
END
$policies$;
GRANT SELECT,INSERT,UPDATE ON tombot.computer_execution_heads,tombot.computer_sessions TO tombot_tenant;
CREATE POLICY dispatch_computer_sessions ON tombot.computer_sessions TO tombot_dispatch USING(true);
GRANT SELECT(user_id,id,job_id,run_id,bot_id,job_owner,job_fencing_token,job_attempt,status,last_checked_at,next_check_at,cleanup_attempt) ON tombot.computer_sessions TO tombot_dispatch;
CREATE POLICY control_computer_sessions ON tombot.computer_sessions TO tombot_control USING(true);
GRANT SELECT ON tombot.computer_sessions TO tombot_control;

-- 任务提交终态或换代不能绕过电脑回收；普通更新与续约不触发额外查询。
CREATE FUNCTION tombot.guard_job_computer_release() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF (NEW.status<>'leased' OR NEW.fencing_token<>OLD.fencing_token OR NEW.lease_owner IS DISTINCT FROM OLD.lease_owner)
       AND EXISTS(SELECT 1 FROM tombot.computer_sessions s WHERE s.user_id=OLD.user_id AND s.job_id=OLD.id AND s.status<>'released') THEN
        RAISE EXCEPTION '电脑会话尚未确认释放' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.guard_job_computer_release() FROM PUBLIC;
CREATE TRIGGER jobs_computer_release BEFORE UPDATE ON tombot.jobs FOR EACH ROW EXECUTE FUNCTION tombot.guard_job_computer_release();
