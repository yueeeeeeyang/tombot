-- 每只猫咪独立保存结构化记忆；不把旧聊天记录自动回填为长期事实。
-- 新表为空，新增可空或常量列不重写历史正文；迁移事务失败整体回滚。
ALTER TABLE tombot.bots ADD COLUMN memory_forget_sequence bigint NOT NULL DEFAULT 0 CHECK (memory_forget_sequence>=0);
ALTER TABLE tombot.runs ADD COLUMN memory_context text CHECK (octet_length(memory_context)<=8192);

CREATE TABLE tombot.bot_memories (
    user_id text NOT NULL,
    bot_id text NOT NULL,
    key text NOT NULL CHECK (key ~ '^[a-z][a-z0-9_.-]{0,79}$'),
    category text NOT NULL CHECK (category IN ('preference','habit','workflow','project')),
    summary text NOT NULL CHECK (octet_length(summary)<=128),
    content text NOT NULL CHECK (octet_length(content)<=1024),
    revision bigint NOT NULL CHECK (revision>0),
    source_message_id text NOT NULL,
    source_sequence bigint NOT NULL CHECK (source_sequence>0),
    is_deleted boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id,bot_id,key),
    FOREIGN KEY (user_id,bot_id) REFERENCES tombot.bots(user_id,id),
    FOREIGN KEY (user_id,source_message_id) REFERENCES tombot.messages(user_id,id),
    CHECK ((is_deleted AND summary='' AND content='') OR (NOT is_deleted AND summary<>'' AND content<>''))
);
ALTER TABLE tombot.bot_memories ENABLE ROW LEVEL SECURITY;
ALTER TABLE tombot.bot_memories FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON tombot.bot_memories TO tombot_tenant
    USING (user_id=nullif(current_setting('tombot.user_id',true),''))
    WITH CHECK (user_id=nullif(current_setting('tombot.user_id',true),''));
GRANT SELECT,INSERT,UPDATE,DELETE ON tombot.bot_memories TO tombot_tenant;
