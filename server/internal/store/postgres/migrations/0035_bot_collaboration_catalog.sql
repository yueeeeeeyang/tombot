-- 机器人与群组采用资源级修改；不读取旧工作空间或建立客户端身份映射。
ALTER TABLE tombot.bots
    ADD COLUMN approval_mode text NOT NULL DEFAULT 'standard' CHECK (approval_mode IN ('loose','standard','strict')),
    ADD COLUMN accent_hue integer NOT NULL DEFAULT 0 CHECK (accent_hue BETWEEN 0 AND 359),
    ADD COLUMN client_request_id text,
    ADD COLUMN creation_digest bytea,
    ADD CONSTRAINT bots_creation_request CHECK (
        (client_request_id IS NULL AND creation_digest IS NULL) OR
        (client_request_id IS NOT NULL AND client_request_id ~ '^[0-9A-HJKMNP-TV-Z]{26}$' AND octet_length(creation_digest)=32 AND creation_digest IS NOT NULL)),
    ADD CONSTRAINT bots_client_request UNIQUE (user_id, client_request_id);

ALTER TABLE tombot.groups
    ADD COLUMN is_archived boolean NOT NULL DEFAULT false,
    ADD COLUMN max_cost_micro bigint CHECK (max_cost_micro BETWEEN 1 AND 1000000000000),
    ADD COLUMN client_request_id text,
    ADD COLUMN creation_digest bytea,
    ADD CONSTRAINT groups_creation_request CHECK (
        (client_request_id IS NULL AND creation_digest IS NULL) OR
        (client_request_id IS NOT NULL AND client_request_id ~ '^[0-9A-HJKMNP-TV-Z]{26}$' AND octet_length(creation_digest)=32 AND creation_digest IS NOT NULL)),
    ADD CONSTRAINT groups_client_request UNIQUE (user_id, client_request_id);
CREATE INDEX groups_page ON tombot.groups(user_id, is_archived, id DESC);
CREATE INDEX bots_page ON tombot.bots(user_id, is_archived, id DESC);

-- 三种会话都只关联同用户资源，机器人私信以排序后的双方身份唯一定位。
CREATE TABLE tombot.conversations (
    user_id text NOT NULL REFERENCES tombot.users(id),
    id text NOT NULL CHECK (id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    kind text NOT NULL CHECK (kind IN ('direct','group','bot_dm')),
    bot_id text,
    group_id text,
    peer_bot_id text,
    title text NOT NULL CHECK (length(btrim(title)) BETWEEN 1 AND 120),
    summary text NOT NULL DEFAULT '',
    message_sequence bigint NOT NULL DEFAULT 0 CHECK (message_sequence >= 0),
    last_read_sequence bigint NOT NULL DEFAULT 0 CHECK (last_read_sequence BETWEEN 0 AND message_sequence),
    is_archived boolean NOT NULL DEFAULT false,
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id,id),
    FOREIGN KEY (user_id,bot_id) REFERENCES tombot.bots(user_id,id),
    FOREIGN KEY (user_id,peer_bot_id) REFERENCES tombot.bots(user_id,id),
    FOREIGN KEY (user_id,group_id) REFERENCES tombot.groups(user_id,id),
    CHECK ((kind='direct' AND bot_id IS NOT NULL AND group_id IS NULL AND peer_bot_id IS NULL) OR
        (kind='group' AND group_id IS NOT NULL AND bot_id IS NULL AND peer_bot_id IS NULL) OR
        (kind='bot_dm' AND bot_id IS NOT NULL AND peer_bot_id IS NOT NULL AND group_id IS NULL AND bot_id<peer_bot_id))
);
CREATE UNIQUE INDEX conversations_direct ON tombot.conversations(user_id,bot_id) WHERE kind='direct';
CREATE UNIQUE INDEX conversations_group ON tombot.conversations(user_id,group_id) WHERE kind='group';
CREATE UNIQUE INDEX conversations_bot_dm ON tombot.conversations(user_id,bot_id,peer_bot_id) WHERE kind='bot_dm';
CREATE INDEX conversations_page ON tombot.conversations(user_id,is_archived,id DESC);

-- 小尺寸头像属于用户配置，不进入共享电脑目录，防止其他任务覆盖展示资产。
CREATE TABLE tombot.bot_avatars (
    user_id text NOT NULL,
    bot_id text NOT NULL,
    content bytea NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 73728),
    sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id,bot_id),
    FOREIGN KEY (user_id,bot_id) REFERENCES tombot.bots(user_id,id)
);

DO $policies$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['conversations','bot_avatars'] LOOP
        EXECUTE format('ALTER TABLE tombot.%I ENABLE ROW LEVEL SECURITY', table_name);
        EXECUTE format('ALTER TABLE tombot.%I FORCE ROW LEVEL SECURITY', table_name);
        EXECUTE format('CREATE POLICY tenant_scope ON tombot.%I TO tombot_tenant USING (user_id=nullif(current_setting(''tombot.user_id'',true),'''')) WITH CHECK (user_id=nullif(current_setting(''tombot.user_id'',true),''''))', table_name);
        EXECUTE format('GRANT SELECT,INSERT,UPDATE,DELETE ON tombot.%I TO tombot_tenant', table_name);
    END LOOP;
END
$policies$;
