-- 管理员与普通用户是两套身份：管理员只做平台管理，不持有用户电脑，也不登录桌面客户端。
-- 拆分后 users 只保存普通用户，administrators 保存管理员；两者的会话表互不复用。

CREATE TABLE tombot.administrators (
    id text PRIMARY KEY CHECK (id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    username text NOT NULL UNIQUE CHECK (username = lower(btrim(username)) AND length(username) BETWEEN 3 AND 64 AND username ~ '^[a-z0-9_.-]{3,64}$'),
    display_name text NOT NULL CHECK (length(btrim(display_name)) BETWEEN 1 AND 120),
    status text NOT NULL CHECK (status IN ('pending_activation', 'active', 'disabled')),
    auth_version bigint NOT NULL DEFAULT 1 CHECK (auth_version > 0),
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE tombot.administrator_credentials (
    administrator_id text PRIMARY KEY REFERENCES tombot.administrators(id),
    password_hash text NOT NULL CHECK (length(password_hash) > 0),
    changed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE tombot.administrator_devices (
    administrator_id text NOT NULL REFERENCES tombot.administrators(id),
    id text NOT NULL,
    name text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 120),
    platform text NOT NULL CHECK (platform = 'web'),
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_seen_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (administrator_id, id)
);

CREATE TABLE tombot.administrator_sessions (
    administrator_id text NOT NULL REFERENCES tombot.administrators(id),
    id text NOT NULL UNIQUE,
    device_id text NOT NULL,
    auth_version bigint NOT NULL CHECK (auth_version > 0),
    -- 管理员会话恒为管理会话；保留该列使两套身份可以共用同一组语句模板。
    scope text NOT NULL DEFAULT 'admin' CHECK (scope = 'admin'),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    verified_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (administrator_id, id),
    FOREIGN KEY (administrator_id, device_id) REFERENCES tombot.administrator_devices(administrator_id, id),
    CHECK (expires_at > created_at)
);
CREATE INDEX administrator_sessions_active ON tombot.administrator_sessions(administrator_id, expires_at) WHERE revoked_at IS NULL;

CREATE TABLE tombot.administrator_session_tokens (
    administrator_id text NOT NULL,
    id text NOT NULL,
    session_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('access', 'refresh')),
    token_digest bytea NOT NULL UNIQUE CHECK (octet_length(token_digest) = 32),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (administrator_id, id),
    FOREIGN KEY (administrator_id, session_id) REFERENCES tombot.administrator_sessions(administrator_id, id),
    CHECK (expires_at > created_at)
);
CREATE INDEX administrator_session_tokens_expiry ON tombot.administrator_session_tokens(expires_at);

CREATE TABLE tombot.administrator_mfa (
    administrator_id text PRIMARY KEY REFERENCES tombot.administrators(id),
    ciphertext bytea NOT NULL,
    nonce bytea NOT NULL CHECK (octet_length(nonce) = 12),
    last_step bigint NOT NULL DEFAULT -1 CHECK (last_step >= -1),
    enrolled_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE tombot.administrator_activation_tokens (
    id text PRIMARY KEY,
    administrator_id text NOT NULL REFERENCES tombot.administrators(id),
    token_digest bytea NOT NULL UNIQUE CHECK (octet_length(token_digest) = 32),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (expires_at > created_at)
);
CREATE UNIQUE INDEX administrator_activation_pending ON tombot.administrator_activation_tokens(administrator_id) WHERE consumed_at IS NULL;

CREATE TABLE tombot.administrator_activation_mfa (
    activation_id text PRIMARY KEY REFERENCES tombot.administrator_activation_tokens(id),
    administrator_id text NOT NULL REFERENCES tombot.administrators(id),
    ciphertext bytea NOT NULL,
    nonce bytea NOT NULL CHECK (octet_length(nonce) = 12),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- 迁移现有管理员身份。管理员从不持有产品数据，因此可以连同电脑与会话一起删除；
-- 若某个管理员仍拥有机器人等业务记录，删除会因外键失败并整体回滚，由部署先处理数据。
INSERT INTO tombot.administrators(id, username, display_name, status, auth_version, revision, created_at, updated_at)
SELECT id, username, display_name,
    CASE WHEN status IN ('pending_activation', 'active', 'disabled') THEN status ELSE 'pending_activation' END,
    auth_version, revision, created_at, updated_at
FROM tombot.users WHERE role = 'admin';

INSERT INTO tombot.administrator_credentials(administrator_id, password_hash, changed_at)
SELECT user_id, password_hash, changed_at FROM tombot.user_credentials WHERE user_id IN (SELECT id FROM tombot.administrators);

INSERT INTO tombot.administrator_mfa(administrator_id, ciphertext, nonce, last_step, enrolled_at)
SELECT user_id, ciphertext, nonce, last_step, enrolled_at FROM tombot.user_mfa WHERE user_id IN (SELECT id FROM tombot.administrators);

INSERT INTO tombot.administrator_activation_tokens(id, administrator_id, token_digest, expires_at, consumed_at, created_at)
SELECT id, user_id, token_digest, expires_at, consumed_at, created_at FROM tombot.activation_tokens WHERE user_id IN (SELECT id FROM tombot.administrators);

-- 先解除跨身份引用，再删除管理员在用户侧的全部记录。
ALTER TABLE tombot.admin_audit_events DROP CONSTRAINT admin_audit_events_actor_id_fkey;
ALTER TABLE tombot.user_creation_operations DROP CONSTRAINT user_creation_operations_actor_id_fkey;
ALTER TABLE tombot.authentication_events DROP CONSTRAINT authentication_events_user_id_fkey;

DELETE FROM tombot.session_tokens WHERE user_id IN (SELECT id FROM tombot.administrators);
DELETE FROM tombot.user_sessions WHERE user_id IN (SELECT id FROM tombot.administrators);
DELETE FROM tombot.devices WHERE user_id IN (SELECT id FROM tombot.administrators);
DELETE FROM tombot.activation_mfa WHERE user_id IN (SELECT id FROM tombot.administrators);
DELETE FROM tombot.activation_tokens WHERE user_id IN (SELECT id FROM tombot.administrators);
DELETE FROM tombot.user_mfa WHERE user_id IN (SELECT id FROM tombot.administrators);
DELETE FROM tombot.user_creation_operations WHERE user_id IN (SELECT id FROM tombot.administrators);
-- 管理员不再占用电脑容量；释放预留后删除电脑记录。
UPDATE tombot.computer_nodes n SET reserved = n.reserved - owned.count
    FROM (SELECT node_id, count(*) AS count FROM tombot.computers WHERE user_id IN (SELECT id FROM tombot.administrators) GROUP BY node_id) owned
    WHERE n.id = owned.node_id;
DELETE FROM tombot.computers WHERE user_id IN (SELECT id FROM tombot.administrators);
DELETE FROM tombot.user_limits WHERE user_id IN (SELECT id FROM tombot.administrators);
DELETE FROM tombot.user_model_assignments WHERE user_id IN (SELECT id FROM tombot.administrators);
DELETE FROM tombot.user_event_heads WHERE user_id IN (SELECT id FROM tombot.administrators);
DELETE FROM tombot.user_events WHERE user_id IN (SELECT id FROM tombot.administrators);
DELETE FROM tombot.user_preferences WHERE user_id IN (SELECT id FROM tombot.administrators);
DELETE FROM tombot.user_workspaces WHERE user_id IN (SELECT id FROM tombot.administrators);
DELETE FROM tombot.user_credentials WHERE user_id IN (SELECT id FROM tombot.administrators);

-- 管理员只剩普通用户语义，角色列不再需要；最后一名管理员的保护转移到 administrators。
DROP TRIGGER users_last_administrator ON tombot.users;
DROP INDEX tombot.users_active_administrators;
-- 0049 的免电脑规则以 users.role 判定，拆表后该前提消失，必须一并移除并恢复非空约束。
DROP TRIGGER user_creation_admin_without_computer ON tombot.user_creation_operations;
DROP FUNCTION tombot.check_admin_without_computer();
ALTER TABLE tombot.user_creation_operations
    DROP CONSTRAINT user_creation_computer_or_reserved,
    ALTER COLUMN computer_id SET NOT NULL;
ALTER TABLE tombot.users DROP COLUMN role;
DELETE FROM tombot.users WHERE id IN (SELECT id FROM tombot.administrators);

CREATE FUNCTION tombot.protect_last_active_administrator() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF OLD.status='active' THEN
        IF TG_OP='UPDATE' AND NEW.status='active' THEN
            RETURN NEW;
        END IF;
        PERFORM pg_advisory_xact_lock(hashtextextended('tombot.active_administrators',0));
        IF NOT EXISTS (SELECT 1 FROM tombot.administrators WHERE status='active' AND id<>OLD.id) THEN
            RAISE EXCEPTION '不能移除最后一名有效管理员' USING ERRCODE='23514';
        END IF;
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.protect_last_active_administrator() FROM PUBLIC;
CREATE TRIGGER administrators_last_active BEFORE UPDATE OF status OR DELETE ON tombot.administrators
    FOR EACH ROW EXECUTE FUNCTION tombot.protect_last_active_administrator();

-- 审计与开户操作的行为主体可能是管理员，也可能是内部命令，因此保留文本标识而不强制单表外键。
CREATE INDEX admin_audit_actor ON tombot.admin_audit_events(actor_id) WHERE actor_id IS NOT NULL;

ALTER TABLE tombot.administrators ENABLE ROW LEVEL SECURITY;
ALTER TABLE tombot.administrators FORCE ROW LEVEL SECURITY;
CREATE POLICY identity_administrators ON tombot.administrators TO tombot_identity USING (true) WITH CHECK (true);
CREATE POLICY control_administrators ON tombot.administrators TO tombot_control USING (true) WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE ON tombot.administrators TO tombot_identity;
GRANT SELECT, INSERT, UPDATE, DELETE ON tombot.administrator_credentials, tombot.administrator_devices,
    tombot.administrator_sessions, tombot.administrator_session_tokens, tombot.administrator_mfa,
    tombot.administrator_activation_tokens, tombot.administrator_activation_mfa TO tombot_identity;
-- 控制连接只负责开户与签发激活材料，不能读取管理员密码或管理会话。
GRANT SELECT, INSERT, UPDATE ON tombot.administrators TO tombot_control;
GRANT SELECT, INSERT, UPDATE ON tombot.administrator_activation_tokens TO tombot_control;
CREATE POLICY control_administrator_activations ON tombot.administrator_activation_tokens TO tombot_control USING (true) WITH CHECK (true);

-- 用户侧不再接受管理会话；历史管理会话已随管理员注销而删除。
ALTER TABLE tombot.user_sessions ADD CONSTRAINT user_sessions_scope CHECK (scope = 'desktop');
