-- 当前标记与不可变请求回执分开保存；同一请求重放不能撤销后续操作。
-- 不回填旧本地表情。新表为空，新增常量列不重写历史正文；事务失败整体回滚。
ALTER TABLE tombot.messages ADD COLUMN reaction_revision bigint NOT NULL DEFAULT 0 CHECK (reaction_revision>=0);
GRANT UPDATE(reaction_revision) ON tombot.messages TO tombot_tenant;

CREATE TABLE tombot.message_reactions (
    user_id text NOT NULL,
    message_id text NOT NULL,
    emoji text NOT NULL CHECK (octet_length(emoji) BETWEEN 1 AND 32),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id,message_id,emoji),
    FOREIGN KEY (user_id,message_id) REFERENCES tombot.messages(user_id,id)
);
CREATE TABLE tombot.message_reaction_requests (
    user_id text NOT NULL,
    client_request_id text NOT NULL CHECK (client_request_id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    message_id text NOT NULL,
    emoji text NOT NULL CHECK (octet_length(emoji) BETWEEN 1 AND 32),
    is_active boolean NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id,client_request_id),
    FOREIGN KEY (user_id,message_id) REFERENCES tombot.messages(user_id,id)
);

DO $policies$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['message_reactions','message_reaction_requests'] LOOP
        EXECUTE format('ALTER TABLE tombot.%I ENABLE ROW LEVEL SECURITY',table_name);
        EXECUTE format('ALTER TABLE tombot.%I FORCE ROW LEVEL SECURITY',table_name);
        EXECUTE format('CREATE POLICY tenant_scope ON tombot.%I TO tombot_tenant USING (user_id=nullif(current_setting(''tombot.user_id'',true),'''')) WITH CHECK (user_id=nullif(current_setting(''tombot.user_id'',true),''''))',table_name);
    END LOOP;
END
$policies$;
GRANT SELECT,INSERT,DELETE ON tombot.message_reactions TO tombot_tenant;
GRANT SELECT,INSERT ON tombot.message_reaction_requests TO tombot_tenant;
