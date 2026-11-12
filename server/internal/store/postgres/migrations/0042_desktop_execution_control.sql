-- 最终执行模型从空记录初始化，不把旧电脑独占代次解释成桌面代次。
DO $empty_execution$
BEGIN
    IF EXISTS(SELECT 1 FROM tombot.computer_sessions) OR EXISTS(SELECT 1 FROM tombot.user_limits) THEN
        RAISE EXCEPTION '桌面执行模型需要空会话及用户限额，不支持中间模型数据转换' USING ERRCODE='55000';
    END IF;
END
$empty_execution$;

ALTER TABLE tombot.user_limits ADD COLUMN concurrent_desktops integer NOT NULL CHECK(concurrent_desktops BETWEEN 1 AND 64);
ALTER TABLE tombot.computer_execution_heads DROP COLUMN last_fencing_token;
ALTER TABLE tombot.bot_desktops ADD CONSTRAINT bot_desktops_execution_binding UNIQUE(user_id,id,bot_id,computer_id,linux_uid,shared_gid,display_number);

CREATE TABLE tombot.desktop_execution_heads (
    user_id text NOT NULL,
    desktop_id text NOT NULL,
    last_fencing_token bigint NOT NULL DEFAULT 0 CHECK(last_fencing_token>=0),
    PRIMARY KEY(user_id,desktop_id),
    FOREIGN KEY(user_id,desktop_id) REFERENCES tombot.bot_desktops(user_id,id)
);
ALTER TABLE tombot.desktop_execution_heads ENABLE ROW LEVEL SECURITY;
ALTER TABLE tombot.desktop_execution_heads FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON tombot.desktop_execution_heads TO tombot_tenant
    USING(user_id=nullif(current_setting('tombot.user_id',true),'')) WITH CHECK(user_id=nullif(current_setting('tombot.user_id',true),''));
GRANT SELECT,INSERT,UPDATE ON tombot.desktop_execution_heads TO tombot_tenant;

ALTER TABLE tombot.computer_sessions
    ADD COLUMN desktop_id text NOT NULL,
    ADD COLUMN linux_uid integer NOT NULL,
    ADD COLUMN shared_gid integer NOT NULL,
    ADD COLUMN display_number integer NOT NULL,
    ADD COLUMN wait_reason text NOT NULL DEFAULT '' CHECK(wait_reason IN ('','pending','desktop_busy','user_capacity')),
    ADD CONSTRAINT computer_sessions_wait_reason CHECK((status='waiting')=(wait_reason<>'')),
    ADD CONSTRAINT computer_sessions_desktop_owner FOREIGN KEY(user_id,desktop_id,bot_id,computer_id,linux_uid,shared_gid,display_number)
        REFERENCES tombot.bot_desktops(user_id,id,bot_id,computer_id,linux_uid,shared_gid,display_number),
    DROP CONSTRAINT computer_sessions_user_id_fencing_token_key;
ALTER TABLE tombot.computer_sessions ADD CONSTRAINT computer_sessions_desktop_fence UNIQUE(user_id,desktop_id,fencing_token);
DROP INDEX tombot.computer_sessions_exclusive;
CREATE UNIQUE INDEX computer_sessions_desktop_exclusive ON tombot.computer_sessions(user_id,desktop_id)
    WHERE status IN ('starting','active','draining','quarantined');
CREATE INDEX computer_sessions_desktop_queue ON tombot.computer_sessions(user_id,desktop_id,ticket) WHERE status='waiting';

CREATE OR REPLACE FUNCTION tombot.protect_computer_session() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF (NEW.user_id,NEW.id,NEW.computer_id,NEW.node_id,NEW.container_id,NEW.generation,NEW.job_id,NEW.run_id,NEW.bot_id,
        NEW.desktop_id,NEW.linux_uid,NEW.shared_gid,NEW.display_number,NEW.job_owner,NEW.job_fencing_token,NEW.job_attempt,NEW.ticket,NEW.wait_deadline_at,NEW.created_at)
       IS DISTINCT FROM
       (OLD.user_id,OLD.id,OLD.computer_id,OLD.node_id,OLD.container_id,OLD.generation,OLD.job_id,OLD.run_id,OLD.bot_id,
        OLD.desktop_id,OLD.linux_uid,OLD.shared_gid,OLD.display_number,OLD.job_owner,OLD.job_fencing_token,OLD.job_attempt,OLD.ticket,OLD.wait_deadline_at,OLD.created_at)
       OR (OLD.fencing_token IS NOT NULL AND (NEW.fencing_token,NEW.hold_deadline_at) IS DISTINCT FROM (OLD.fencing_token,OLD.hold_deadline_at))
       OR NEW.revision<OLD.revision
       OR (OLD.status='released' AND NEW IS DISTINCT FROM OLD)
       OR (NEW.status<>OLD.status AND NOT (
           (OLD.status='waiting' AND NEW.status IN ('starting','released')) OR
           (OLD.status='starting' AND NEW.status IN ('active','draining','quarantined')) OR
           (OLD.status='active' AND NEW.status IN ('draining','quarantined')) OR
           (OLD.status IN ('draining','quarantined') AND NEW.status IN ('draining','quarantined','released')))) THEN
        RAISE EXCEPTION '桌面会话身份或状态不可改写' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;

-- 准入与释放持有同一用户短事务锁，清理未知的桌面仍计入容量。
CREATE FUNCTION tombot.guard_desktop_capacity() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
DECLARE maximum integer;
BEGIN
    IF NEW.status IN ('starting','active','draining','quarantined') AND
       (TG_OP='INSERT' OR OLD.status NOT IN ('starting','active','draining','quarantined')) THEN
        PERFORM 1 FROM tombot.computer_execution_heads WHERE user_id=NEW.user_id FOR UPDATE;
        IF NOT FOUND THEN
            RAISE EXCEPTION '缺少桌面执行准入记录' USING ERRCODE='23514';
        END IF;
        SELECT concurrent_desktops INTO maximum FROM tombot.user_limits WHERE user_id=NEW.user_id;
        IF maximum IS NULL OR (SELECT count(*) FROM tombot.computer_sessions WHERE user_id=NEW.user_id AND id<>NEW.id
            AND status IN ('starting','active','draining','quarantined'))>=maximum THEN
            RAISE EXCEPTION '用户桌面并发额度已满或未配置' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.guard_desktop_capacity() FROM PUBLIC;
CREATE TRIGGER computer_sessions_capacity BEFORE INSERT OR UPDATE ON tombot.computer_sessions FOR EACH ROW EXECUTE FUNCTION tombot.guard_desktop_capacity();

CREATE FUNCTION tombot.protect_desktop_execution_head() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF TG_OP='DELETE' OR (NEW.user_id,NEW.desktop_id) IS DISTINCT FROM (OLD.user_id,OLD.desktop_id)
       OR NEW.last_fencing_token<OLD.last_fencing_token THEN
        RAISE EXCEPTION '桌面控制代次不可删除、更换或倒退' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.protect_desktop_execution_head() FROM PUBLIC;
CREATE TRIGGER desktop_execution_heads_fixed BEFORE UPDATE OR DELETE ON tombot.desktop_execution_heads FOR EACH ROW EXECUTE FUNCTION tombot.protect_desktop_execution_head();
