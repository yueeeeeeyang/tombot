-- 浏览器登录把凭据输入放在系统浏览器里，桌面端只接收一次性授权码。
-- 授权记录不保存口令、访问凭据或续期凭据；凭据在交换授权码时才签发。
CREATE TABLE tombot.desktop_authorizations (
    id text PRIMARY KEY CHECK (id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    -- state 只用于桌面端校验回调来源，不是凭据；长度限制防止无限输入。
    state text NOT NULL CHECK (length(state) BETWEEN 16 AND 128),
    redirect_uri text NOT NULL CHECK (length(redirect_uri) BETWEEN 1 AND 2048),
    device_name text NOT NULL CHECK (length(device_name) BETWEEN 1 AND 120),
    platform text NOT NULL CHECK (platform IN ('macos','windows','linux')),
    -- PKCE：授权码与校验串只以 SHA-256 摘要保存，数据库泄漏不能直接换取会话。
    challenge_digest bytea NOT NULL CHECK (octet_length(challenge_digest)=32),
    code_digest bytea UNIQUE CHECK (code_digest IS NULL OR octet_length(code_digest)=32),
    user_id text REFERENCES tombot.users(id),
    approved_at timestamptz,
    consumed_at timestamptz,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    -- 只有完成口令与动态验证之后才写入用户和授权码摘要；两者必须同时出现。
    CHECK ((approved_at IS NULL) = (code_digest IS NULL)),
    CHECK ((approved_at IS NULL) = (user_id IS NULL)),
    CHECK (consumed_at IS NULL OR approved_at IS NOT NULL)
);
-- 过期清理按到期时间扫描，避免全表顺序读取。
CREATE INDEX desktop_authorizations_expiry ON tombot.desktop_authorizations(expires_at);

-- 授权记录由身份连接读写，不暴露给租户连接；用户内容不能借授权码旁路读取。
GRANT SELECT, INSERT, UPDATE, DELETE ON tombot.desktop_authorizations TO tombot_identity;
