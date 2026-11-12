-- 截图与工具终态在同一事务中提交；像素不进入操作 JSON、日志或 Agent 控制帧。
ALTER TABLE tombot.run_operations ADD COLUMN sequence bigint GENERATED ALWAYS AS IDENTITY;
CREATE FUNCTION tombot.protect_operation_sequence() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF NEW.sequence IS DISTINCT FROM OLD.sequence THEN
        RAISE EXCEPTION '操作序号不可改写' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.protect_operation_sequence() FROM PUBLIC;
CREATE TRIGGER run_operations_sequence_fixed BEFORE UPDATE ON tombot.run_operations
    FOR EACH ROW EXECUTE FUNCTION tombot.protect_operation_sequence();
GRANT USAGE ON SEQUENCE tombot.run_operations_sequence_seq TO tombot_tenant;

CREATE TABLE tombot.run_screenshots (
    user_id text NOT NULL,
    id text NOT NULL,
    run_id text NOT NULL,
    bot_id text NOT NULL,
    fencing_token bigint NOT NULL,
    attempt integer NOT NULL,
    sha256 bytea NOT NULL CHECK(octet_length(sha256)=32),
    content bytea NOT NULL CHECK(octet_length(content) BETWEEN 1 AND 4194304),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL CHECK(expires_at>created_at),
    PRIMARY KEY(user_id,id),
    FOREIGN KEY(user_id,id,run_id,fencing_token,attempt) REFERENCES tombot.run_operations(user_id,id,run_id,fencing_token,attempt),
    FOREIGN KEY(user_id,run_id,bot_id) REFERENCES tombot.runs(user_id,id,bot_id)
);
CREATE INDEX run_screenshots_expiry ON tombot.run_screenshots(expires_at,user_id,id);
CREATE INDEX run_screenshots_run ON tombot.run_screenshots(user_id,run_id,id);
ALTER TABLE tombot.run_screenshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE tombot.run_screenshots FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON tombot.run_screenshots TO tombot_tenant
    USING(user_id=nullif(current_setting('tombot.user_id',true),''))
    WITH CHECK(user_id=nullif(current_setting('tombot.user_id',true),''));
GRANT SELECT,INSERT ON tombot.run_screenshots TO tombot_tenant;
-- 清理者只能读取身份与到期时间，不能读取截图像素；运行中和待重试任务由清理查询排除。
CREATE POLICY dispatch_screenshots ON tombot.run_screenshots TO tombot_dispatch USING(true);
GRANT SELECT(user_id,id,run_id,expires_at),DELETE,UPDATE(expires_at) ON tombot.run_screenshots TO tombot_dispatch;
CREATE FUNCTION tombot.protect_screenshot() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    RAISE EXCEPTION '截图内容与身份不可改写' USING ERRCODE='23514';
END
$function$;
REVOKE ALL ON FUNCTION tombot.protect_screenshot() FROM PUBLIC;
-- UPDATE 列权限只供清理查询取得行锁，任何实际更新均被触发器拒绝。
CREATE TRIGGER run_screenshots_fixed BEFORE UPDATE ON tombot.run_screenshots FOR EACH ROW EXECUTE FUNCTION tombot.protect_screenshot();
