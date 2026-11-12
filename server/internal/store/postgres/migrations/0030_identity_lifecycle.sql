-- 身份模块只可激活待激活账户或更新有效账户的认证版本，不能创建、提权或禁用账户。
GRANT UPDATE (status, auth_version, revision, updated_at) ON tombot.users TO tombot_identity;
CREATE POLICY identity_user_update ON tombot.users FOR UPDATE TO tombot_identity
    USING (status IN ('pending_activation', 'active')) WITH CHECK (status = 'active');
GRANT SELECT (user_id, status) ON tombot.computers TO tombot_identity;
CREATE POLICY identity_computer_read ON tombot.computers FOR SELECT TO tombot_identity USING (true);

-- 创建成功与首次激活均以实际电脑就绪为前置，不能靠 HTTP 流程顺序维持约束。
CREATE FUNCTION tombot.check_user_activation() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, tombot, pg_temp AS $function$
BEGIN
    IF NEW.status IN ('pending_activation', 'active') AND
       (TG_OP = 'INSERT' OR OLD.status NOT IN ('active', 'disabled')) THEN
        IF NOT EXISTS (SELECT 1 FROM tombot.computers WHERE user_id = NEW.id AND status = 'ready') THEN
            RAISE EXCEPTION '用户电脑尚未就绪' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.check_user_activation() FROM PUBLIC;
CREATE TRIGGER users_activation_ready BEFORE INSERT OR UPDATE OF status ON tombot.users
    FOR EACH ROW EXECUTE FUNCTION tombot.check_user_activation();

ALTER TABLE tombot.user_sessions ADD COLUMN verified_at timestamptz;
CREATE INDEX session_tokens_session ON tombot.session_tokens(user_id, session_id, kind);
CREATE INDEX devices_active ON tombot.devices(user_id, created_at DESC, id DESC) WHERE revoked_at IS NULL;
CREATE INDEX auth_rate_limits_expiry ON tombot.auth_rate_limits(expires_at);

-- 多因素材料只以密文保存；已使用的时间步持久化，阻断并发重放。
CREATE TABLE tombot.user_mfa (
    user_id text PRIMARY KEY REFERENCES tombot.users(id),
    ciphertext bytea NOT NULL,
    nonce bytea NOT NULL CHECK (octet_length(nonce) = 12),
    last_step bigint NOT NULL DEFAULT -1 CHECK (last_step >= -1),
    enrolled_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE tombot.activation_mfa (
    activation_id text PRIMARY KEY REFERENCES tombot.activation_tokens(id),
    user_id text NOT NULL REFERENCES tombot.users(id),
    ciphertext bytea NOT NULL,
    nonce bytea NOT NULL CHECK (octet_length(nonce) = 12),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
GRANT SELECT, INSERT, UPDATE, DELETE ON tombot.user_mfa, tombot.activation_mfa TO tombot_identity;

CREATE TABLE tombot.authentication_events (
    id text PRIMARY KEY,
    user_id text REFERENCES tombot.users(id),
    session_id text,
    action text NOT NULL,
    result text NOT NULL CHECK (result IN ('succeeded', 'denied')),
    request_id text NOT NULL CHECK (length(request_id) BETWEEN 1 AND 120),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX authentication_events_created ON tombot.authentication_events(created_at DESC, id DESC);
GRANT INSERT ON tombot.authentication_events TO tombot_identity;
GRANT SELECT ON tombot.authentication_events TO tombot_control;

-- 该脚本仅新增小型身份表、列和索引。空库初始化在一个事务内执行；失败整体回滚。
-- 已有版本修正使用下一版本脚本，不修改本脚本或将旧 SQLite 数据导入。
