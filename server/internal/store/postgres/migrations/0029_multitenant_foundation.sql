-- 全新多用户数据库直接建立目标基础模型，不执行或导入历史 SQLite 结构。
CREATE SCHEMA tombot;
REVOKE ALL ON SCHEMA tombot FROM PUBLIC;

CREATE TABLE tombot.schema_migrations (
    version bigint PRIMARY KEY,
    name text NOT NULL UNIQUE,
    checksum text NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
    applied_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- 分离租户、认证、管理和调度连接，避免普通请求继承平台权限。
DO $roles$
DECLARE role_name text;
BEGIN
    FOREACH role_name IN ARRAY ARRAY['tombot_tenant', 'tombot_identity', 'tombot_control', 'tombot_dispatch'] LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = role_name) THEN
            EXECUTE format('CREATE ROLE %I NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', role_name);
        END IF;
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = role_name AND
            (rolcanlogin OR rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)) THEN
            RAISE EXCEPTION '数据库权限组配置不符合最小权限要求';
        END IF;
    END LOOP;
END
$roles$;
GRANT USAGE ON SCHEMA tombot TO tombot_tenant, tombot_identity, tombot_control, tombot_dispatch;
GRANT SELECT ON tombot.schema_migrations TO tombot_tenant, tombot_identity, tombot_control, tombot_dispatch;

CREATE TABLE tombot.users (
    id text PRIMARY KEY CHECK (id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    username text NOT NULL UNIQUE CHECK (username = lower(btrim(username)) AND length(username) BETWEEN 3 AND 64),
    display_name text NOT NULL CHECK (length(btrim(display_name)) BETWEEN 1 AND 120),
    role text NOT NULL CHECK (role IN ('admin', 'user')),
    status text NOT NULL CHECK (status IN ('provisioning', 'provision_failed', 'pending_activation', 'active', 'disabled', 'deleting', 'deleted')),
    auth_version bigint NOT NULL DEFAULT 1 CHECK (auth_version > 0),
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE tombot.user_credentials (
    user_id text PRIMARY KEY REFERENCES tombot.users(id),
    password_hash text NOT NULL CHECK (length(password_hash) > 0),
    changed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE tombot.activation_tokens (
    id text PRIMARY KEY,
    user_id text NOT NULL REFERENCES tombot.users(id),
    token_digest bytea NOT NULL UNIQUE CHECK (octet_length(token_digest) = 32),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (expires_at > created_at)
);
CREATE UNIQUE INDEX activation_tokens_pending ON tombot.activation_tokens(user_id) WHERE consumed_at IS NULL;

CREATE TABLE tombot.devices (
    user_id text NOT NULL REFERENCES tombot.users(id),
    id text NOT NULL,
    name text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 120),
    platform text NOT NULL CHECK (platform IN ('macos', 'windows', 'linux', 'web')),
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_seen_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id, id)
);

CREATE TABLE tombot.user_sessions (
    user_id text NOT NULL REFERENCES tombot.users(id),
    id text NOT NULL UNIQUE,
    device_id text NOT NULL,
    auth_version bigint NOT NULL CHECK (auth_version > 0),
    scope text NOT NULL CHECK (scope IN ('desktop', 'admin')),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id, id),
    FOREIGN KEY (user_id, device_id) REFERENCES tombot.devices(user_id, id),
    CHECK (expires_at > created_at)
);
CREATE INDEX user_sessions_active ON tombot.user_sessions(user_id, expires_at) WHERE revoked_at IS NULL;

CREATE TABLE tombot.session_tokens (
    user_id text NOT NULL,
    id text NOT NULL,
    session_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('access', 'refresh')),
    token_digest bytea NOT NULL UNIQUE CHECK (octet_length(token_digest) = 32),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id, id),
    FOREIGN KEY (user_id, session_id) REFERENCES tombot.user_sessions(user_id, id),
    CHECK (expires_at > created_at)
);
CREATE INDEX session_tokens_expiry ON tombot.session_tokens(expires_at);

CREATE TABLE tombot.auth_rate_limits (
    bucket_digest bytea PRIMARY KEY CHECK (octet_length(bucket_digest) = 32),
    failures integer NOT NULL CHECK (failures >= 0),
    blocked_until timestamptz NOT NULL,
    expires_at timestamptz NOT NULL
);

CREATE TABLE tombot.computer_nodes (
    id text PRIMARY KEY,
    name text NOT NULL UNIQUE,
    endpoint text NOT NULL,
    status text NOT NULL CHECK (status IN ('bootstrapping', 'ready', 'draining', 'offline', 'error')),
    capacity integer NOT NULL CHECK (capacity > 0),
    reserved integer NOT NULL DEFAULT 0 CHECK (reserved >= 0 AND reserved <= capacity),
    generation bigint NOT NULL DEFAULT 1 CHECK (generation > 0),
    last_seen_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE tombot.computer_images (
    id text PRIMARY KEY,
    reference text NOT NULL UNIQUE CHECK (reference ~ '@sha256:[0-9a-f]{64}$'),
    is_enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE tombot.computers (
    user_id text PRIMARY KEY REFERENCES tombot.users(id),
    id text NOT NULL UNIQUE,
    node_id text NOT NULL REFERENCES tombot.computer_nodes(id),
    image_id text NOT NULL REFERENCES tombot.computer_images(id),
    status text NOT NULL CHECK (status IN ('pending', 'provisioning', 'ready', 'starting', 'stopping', 'stopped', 'error', 'quarantined', 'deleted')),
    generation bigint NOT NULL DEFAULT 1 CHECK (generation > 0),
    container_id text UNIQUE,
    volume_ref text NOT NULL UNIQUE,
    cpu_millis integer NOT NULL CHECK (cpu_millis > 0),
    memory_bytes bigint NOT NULL CHECK (memory_bytes > 0),
    disk_bytes bigint NOT NULL CHECK (disk_bytes > 0),
    process_limit integer NOT NULL CHECK (process_limit > 0),
    error_code text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (user_id, id),
    CHECK (status <> 'ready' OR container_id IS NOT NULL)
);

CREATE TABLE tombot.user_creation_operations (
    id text PRIMARY KEY,
    user_id text NOT NULL UNIQUE REFERENCES tombot.users(id),
    computer_id text NOT NULL,
    actor_id text REFERENCES tombot.users(id),
    idempotency_key text NOT NULL UNIQUE CHECK (length(idempotency_key) BETWEEN 1 AND 120),
    request_digest bytea NOT NULL CHECK (octet_length(request_digest) = 32),
    status text NOT NULL CHECK (status IN ('pending', 'running', 'reconciling', 'compensating', 'succeeded', 'failed', 'quarantined')),
    stage text NOT NULL,
    attempt integer NOT NULL DEFAULT 1 CHECK (attempt > 0),
    lease_owner text,
    lease_expires_at timestamptz,
    fencing_token bigint NOT NULL DEFAULT 0 CHECK (fencing_token >= 0),
    deadline_at timestamptz NOT NULL,
    error_code text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (user_id, computer_id) REFERENCES tombot.computers(user_id, id),
    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL))
);
CREATE INDEX user_creation_pending ON tombot.user_creation_operations(status, created_at, id)
    WHERE status IN ('pending', 'running', 'reconciling', 'compensating');

CREATE TABLE tombot.bootstrap_operations (
    id text PRIMARY KEY,
    node_id text NOT NULL REFERENCES tombot.computer_nodes(id),
    stage text NOT NULL,
    status text NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'failed')),
    attempt integer NOT NULL DEFAULT 1 CHECK (attempt > 0),
    error_code text,
    started_at timestamptz,
    finished_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE tombot.model_providers (
    id text PRIMARY KEY,
    name text NOT NULL UNIQUE,
    protocol text NOT NULL CHECK (protocol IN ('openai_compat', 'anthropic')),
    base_url text NOT NULL,
    is_enabled boolean NOT NULL DEFAULT true,
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE tombot.model_credentials (
    id text PRIMARY KEY,
    provider_id text NOT NULL REFERENCES tombot.model_providers(id),
    version bigint NOT NULL CHECK (version > 0),
    key_reference text NOT NULL,
    nonce bytea NOT NULL CHECK (octet_length(nonce) > 0),
    ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) > 0),
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (provider_id, version),
    UNIQUE (provider_id, id)
);

CREATE TABLE tombot.models (
    id text PRIMARY KEY,
    provider_id text NOT NULL REFERENCES tombot.model_providers(id),
    model_key text NOT NULL,
    display_name text NOT NULL,
    context_limit integer NOT NULL CHECK (context_limit > 0),
    output_limit integer NOT NULL CHECK (output_limit > 0 AND output_limit <= context_limit),
    supports_tools boolean NOT NULL,
    supports_vision boolean NOT NULL,
    is_enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (provider_id, model_key),
    UNIQUE (provider_id, id)
);

CREATE TABLE tombot.model_policies (
    id text PRIMARY KEY,
    name text NOT NULL UNIQUE,
    is_default boolean NOT NULL DEFAULT false,
    is_enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE UNIQUE INDEX model_policies_default ON tombot.model_policies(is_default) WHERE is_default;

CREATE TABLE tombot.model_policy_versions (
    policy_id text NOT NULL REFERENCES tombot.model_policies(id),
    version bigint NOT NULL CHECK (version > 0),
    provider_id text NOT NULL,
    model_id text NOT NULL,
    credential_id text,
    configuration jsonb NOT NULL CHECK (jsonb_typeof(configuration) = 'object'),
    status text NOT NULL CHECK (status IN ('draft', 'published', 'revoked')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    published_at timestamptz,
    PRIMARY KEY (policy_id, version),
    FOREIGN KEY (provider_id, model_id) REFERENCES tombot.models(provider_id, id),
    FOREIGN KEY (provider_id, credential_id) REFERENCES tombot.model_credentials(provider_id, id),
    CHECK (status <> 'published' OR published_at IS NOT NULL)
);

CREATE TABLE tombot.user_model_assignments (
    user_id text PRIMARY KEY REFERENCES tombot.users(id),
    policy_id text NOT NULL REFERENCES tombot.model_policies(id),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE tombot.bots (
    user_id text NOT NULL REFERENCES tombot.users(id),
    id text NOT NULL,
    name text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 120),
    description text NOT NULL DEFAULT '',
    avatar_ref text NOT NULL DEFAULT '',
    category text NOT NULL DEFAULT '',
    system_prompt text NOT NULL DEFAULT '',
    tool_permissions jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(tool_permissions) = 'array'),
    messaging_policy jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(messaging_policy) = 'object'),
    is_archived boolean NOT NULL DEFAULT false,
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id, id)
);
CREATE UNIQUE INDEX bots_active_name ON tombot.bots(user_id, name) WHERE NOT is_archived;
CREATE INDEX bots_updated ON tombot.bots(user_id, updated_at DESC, id DESC);

CREATE TABLE tombot.bot_model_assignments (
    user_id text NOT NULL,
    bot_id text NOT NULL,
    policy_id text NOT NULL REFERENCES tombot.model_policies(id),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id, bot_id),
    FOREIGN KEY (user_id, bot_id) REFERENCES tombot.bots(user_id, id)
);

CREATE TABLE tombot.user_workspaces (
    user_id text PRIMARY KEY REFERENCES tombot.users(id),
    active_bot_id text,
    primary_bot_id text,
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (user_id, active_bot_id) REFERENCES tombot.bots(user_id, id),
    FOREIGN KEY (user_id, primary_bot_id) REFERENCES tombot.bots(user_id, id)
);

CREATE TABLE tombot.user_preferences (
    user_id text PRIMARY KEY REFERENCES tombot.users(id),
    preferences jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(preferences) = 'object'),
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0)
);

CREATE TABLE tombot.groups (
    user_id text NOT NULL REFERENCES tombot.users(id),
    id text NOT NULL,
    name text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 120),
    purpose text NOT NULL DEFAULT '',
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id, id)
);

CREATE TABLE tombot.group_members (
    user_id text NOT NULL,
    group_id text NOT NULL,
    bot_id text NOT NULL,
    joined_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id, group_id, bot_id),
    FOREIGN KEY (user_id, group_id) REFERENCES tombot.groups(user_id, id),
    FOREIGN KEY (user_id, bot_id) REFERENCES tombot.bots(user_id, id)
);
CREATE INDEX group_members_bot ON tombot.group_members(user_id, bot_id, group_id);

CREATE TABLE tombot.user_limits (
    user_id text PRIMARY KEY REFERENCES tombot.users(id),
    concurrent_model_calls integer NOT NULL CHECK (concurrent_model_calls > 0),
    queued_runs integer NOT NULL CHECK (queued_runs > 0),
    monthly_token_limit bigint CHECK (monthly_token_limit > 0),
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0)
);

CREATE TABLE tombot.user_event_heads (
    user_id text PRIMARY KEY REFERENCES tombot.users(id),
    sequence bigint NOT NULL DEFAULT 0 CHECK (sequence >= 0)
);
CREATE TABLE tombot.user_events (
    user_id text NOT NULL REFERENCES tombot.users(id),
    sequence bigint NOT NULL CHECK (sequence > 0),
    kind text NOT NULL CHECK (length(kind) BETWEEN 1 AND 120),
    resource_id text NOT NULL,
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id, sequence)
);
CREATE INDEX user_events_retention ON tombot.user_events(user_id, created_at, sequence);

CREATE TABLE tombot.admin_audit_events (
    id text PRIMARY KEY,
    actor_id text REFERENCES tombot.users(id),
    action text NOT NULL,
    target_kind text NOT NULL,
    target_id text NOT NULL,
    request_id text NOT NULL,
    result text NOT NULL CHECK (result IN ('succeeded', 'failed', 'unknown')),
    changes jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(changes) = 'object'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX admin_audit_created ON tombot.admin_audit_events(created_at DESC, id DESC);

-- 每张用户表显式启用行级安全；没有事务用户上下文时不匹配任何记录。
DO $policies$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'devices', 'user_sessions', 'session_tokens', 'computers', 'user_model_assignments',
        'bots', 'bot_model_assignments', 'user_workspaces', 'user_preferences', 'groups',
        'group_members', 'user_limits', 'user_event_heads', 'user_events'
    ] LOOP
        EXECUTE format('ALTER TABLE tombot.%I ENABLE ROW LEVEL SECURITY', table_name);
        EXECUTE format('ALTER TABLE tombot.%I FORCE ROW LEVEL SECURITY', table_name);
        EXECUTE format('CREATE POLICY tenant_scope ON tombot.%I TO tombot_tenant USING (user_id = nullif(current_setting(''tombot.user_id'', true), '''')) WITH CHECK (user_id = nullif(current_setting(''tombot.user_id'', true), ''''))', table_name);
    END LOOP;
END
$policies$;

ALTER TABLE tombot.users ENABLE ROW LEVEL SECURITY;
ALTER TABLE tombot.users FORCE ROW LEVEL SECURITY;
CREATE POLICY own_user ON tombot.users FOR SELECT TO tombot_tenant
    USING (id = nullif(current_setting('tombot.user_id', true), ''));
CREATE POLICY identity_user ON tombot.users FOR SELECT TO tombot_identity USING (true);
CREATE POLICY control_user ON tombot.users TO tombot_control USING (true) WITH CHECK (true);

GRANT SELECT ON tombot.users, tombot.devices, tombot.user_sessions, tombot.computers,
    tombot.user_model_assignments, tombot.bot_model_assignments, tombot.user_limits TO tombot_tenant;
GRANT SELECT, INSERT, UPDATE, DELETE ON tombot.bots, tombot.user_workspaces, tombot.user_preferences,
    tombot.groups, tombot.group_members, tombot.user_event_heads, tombot.user_events TO tombot_tenant;

-- 身份连接仅能处理认证数据，不能查看用户机器人、消息或文件。
GRANT SELECT ON tombot.users TO tombot_identity;
GRANT SELECT, INSERT, UPDATE, DELETE ON tombot.user_credentials, tombot.activation_tokens,
    tombot.auth_rate_limits, tombot.devices, tombot.user_sessions, tombot.session_tokens TO tombot_identity;
CREATE POLICY identity_devices ON tombot.devices TO tombot_identity USING (true) WITH CHECK (true);
CREATE POLICY identity_sessions ON tombot.user_sessions TO tombot_identity USING (true) WITH CHECK (true);
CREATE POLICY identity_tokens ON tombot.session_tokens TO tombot_identity USING (true) WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE, DELETE ON tombot.users, tombot.activation_tokens,
    tombot.computer_nodes, tombot.computer_images, tombot.computers, tombot.bootstrap_operations,
    tombot.user_creation_operations, tombot.model_providers, tombot.model_credentials, tombot.models,
    tombot.model_policies, tombot.model_policy_versions, tombot.user_model_assignments,
    tombot.bot_model_assignments, tombot.user_limits TO tombot_control;
GRANT SELECT, UPDATE ON tombot.devices, tombot.user_sessions TO tombot_control;
GRANT SELECT, INSERT ON tombot.admin_audit_events TO tombot_control;
DO $control_policies$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['devices', 'user_sessions', 'computers', 'user_model_assignments', 'bot_model_assignments', 'user_limits'] LOOP
        EXECUTE format('CREATE POLICY control_scope ON tombot.%I TO tombot_control USING (true) WITH CHECK (true)', table_name);
    END LOOP;
END
$control_policies$;
