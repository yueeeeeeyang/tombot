-- 任务内容留在租户表；跨用户调度连接只读取不含正文的到期索引。
CREATE TABLE tombot.scheduled_tasks (
    user_id text NOT NULL,
    id text NOT NULL CHECK (id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    bot_id text NOT NULL,
    conversation_id text NOT NULL,
    name text NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 200),
    prompt text NOT NULL CHECK (octet_length(prompt) BETWEEN 1 AND 4096),
    cron text NOT NULL CHECK (octet_length(cron)<=256),
    timezone text NOT NULL CHECK (octet_length(timezone) BETWEEN 1 AND 100),
    run_at timestamptz,
    is_enabled boolean NOT NULL,
    is_deleted boolean NOT NULL DEFAULT false,
    revision bigint NOT NULL DEFAULT 1 CHECK (revision>0),
    next_run_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(user_id,id),
    FOREIGN KEY(user_id,bot_id) REFERENCES tombot.bots(user_id,id),
    FOREIGN KEY(user_id,conversation_id) REFERENCES tombot.conversations(user_id,id),
    CHECK ((cron<>'' AND run_at IS NULL) OR (cron='' AND run_at IS NOT NULL)),
    CHECK ((is_enabled AND NOT is_deleted)=(next_run_at IS NOT NULL))
);
CREATE INDEX scheduled_tasks_bot ON tombot.scheduled_tasks(user_id,bot_id,id DESC) WHERE NOT is_deleted;
CREATE TABLE tombot.scheduled_task_due (
    user_id text NOT NULL,
    task_id text NOT NULL,
    next_run_at timestamptz NOT NULL,
    PRIMARY KEY(user_id,task_id),
    FOREIGN KEY(user_id,task_id) REFERENCES tombot.scheduled_tasks(user_id,id)
);
CREATE INDEX scheduled_task_due_time ON tombot.scheduled_task_due(next_run_at,user_id,task_id);
CREATE TABLE tombot.scheduled_task_runs (
    user_id text NOT NULL,
    task_id text NOT NULL,
    scheduled_at timestamptz NOT NULL,
    task_revision bigint NOT NULL CHECK (task_revision>0),
    run_id text,
    status text NOT NULL CHECK (status IN ('queued','failed')),
    error_code text NOT NULL DEFAULT '',
    PRIMARY KEY(user_id,task_id,scheduled_at),
    FOREIGN KEY(user_id,task_id) REFERENCES tombot.scheduled_tasks(user_id,id),
    FOREIGN KEY(user_id,run_id) REFERENCES tombot.runs(user_id,id),
    CHECK ((status='queued')=(run_id IS NOT NULL))
);
DO $policies$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['scheduled_tasks','scheduled_task_due','scheduled_task_runs'] LOOP
        EXECUTE format('ALTER TABLE tombot.%I ENABLE ROW LEVEL SECURITY',table_name);
        EXECUTE format('ALTER TABLE tombot.%I FORCE ROW LEVEL SECURITY',table_name);
        EXECUTE format('CREATE POLICY tenant_scope ON tombot.%I TO tombot_tenant USING (user_id=nullif(current_setting(''tombot.user_id'',true),'''')) WITH CHECK (user_id=nullif(current_setting(''tombot.user_id'',true),''''))',table_name);
    END LOOP;
END
$policies$;
GRANT SELECT,INSERT,UPDATE ON tombot.scheduled_tasks,tombot.scheduled_task_runs TO tombot_tenant;
GRANT SELECT,INSERT,UPDATE,DELETE ON tombot.scheduled_task_due TO tombot_tenant;
CREATE POLICY dispatch_tasks ON tombot.scheduled_task_due TO tombot_dispatch USING (true);
GRANT SELECT ON tombot.scheduled_task_due TO tombot_dispatch;

-- 聊天与后台任务使用同一猫咪的不同桌面身份；仍由同一用户的总桌面配额约束。
ALTER TABLE tombot.bot_desktops ADD COLUMN purpose text NOT NULL DEFAULT 'chat' CHECK(purpose IN ('chat','task'));
ALTER TABLE tombot.bot_desktops DROP CONSTRAINT bot_desktops_user_id_bot_id_key;
ALTER TABLE tombot.bot_desktops DROP CONSTRAINT bot_desktops_user_id_bot_id_id_fkey;
ALTER TABLE tombot.bot_desktops ADD CONSTRAINT bot_desktops_bot_owner FOREIGN KEY(user_id,bot_id) REFERENCES tombot.bots(user_id,id) DEFERRABLE INITIALLY DEFERRED;
CREATE UNIQUE INDEX bot_desktops_purpose ON tombot.bot_desktops(user_id,bot_id,purpose);

ALTER TABLE tombot.conversations DROP CONSTRAINT conversations_kind_check;
ALTER TABLE tombot.conversations DROP CONSTRAINT conversations_check1;
ALTER TABLE tombot.conversations ADD CONSTRAINT conversations_kind_check CHECK(kind IN ('direct','group','bot_dm','task'));
ALTER TABLE tombot.conversations ADD CONSTRAINT conversations_target_check CHECK(
 (kind IN ('direct','task') AND bot_id IS NOT NULL AND group_id IS NULL AND peer_bot_id IS NULL) OR
 (kind='group' AND group_id IS NOT NULL AND bot_id IS NULL AND peer_bot_id IS NULL) OR
 (kind='bot_dm' AND bot_id IS NOT NULL AND peer_bot_id IS NOT NULL AND group_id IS NULL AND bot_id<peer_bot_id));
ALTER TABLE tombot.jobs ADD COLUMN execution_kind text NOT NULL DEFAULT 'chat' CHECK(execution_kind IN ('chat','task'));
DROP INDEX tombot.jobs_active_bot;
CREATE UNIQUE INDEX jobs_active_bot ON tombot.jobs(user_id,bot_id,execution_kind) WHERE status='leased';

ALTER TABLE tombot.scheduled_task_runs ADD COLUMN delivery_status text NOT NULL DEFAULT 'pending' CHECK(delivery_status IN ('pending','waiting','delivered'));
ALTER TABLE tombot.scheduled_task_runs ADD COLUMN result_text text NOT NULL DEFAULT '' CHECK(octet_length(result_text)<=65536);
ALTER TABLE tombot.scheduled_task_runs ADD COLUMN delivery_message_id text;
ALTER TABLE tombot.scheduled_task_runs ADD CONSTRAINT scheduled_task_delivery_message FOREIGN KEY(user_id,delivery_message_id) REFERENCES tombot.messages(user_id,id);
CREATE UNIQUE INDEX scheduled_task_run_identity ON tombot.scheduled_task_runs(user_id,run_id) WHERE run_id IS NOT NULL;
CREATE TABLE tombot.scheduled_task_delivery (
 user_id text NOT NULL,
 run_id text NOT NULL,
 task_id text NOT NULL,
 conversation_id text NOT NULL,
 ready_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(user_id,run_id),
 FOREIGN KEY(user_id,run_id) REFERENCES tombot.runs(user_id,id),
 FOREIGN KEY(user_id,task_id) REFERENCES tombot.scheduled_tasks(user_id,id),
 FOREIGN KEY(user_id,conversation_id) REFERENCES tombot.conversations(user_id,id)
);
CREATE INDEX scheduled_task_delivery_queue ON tombot.scheduled_task_delivery(available_at,ready_at,user_id,run_id);
ALTER TABLE tombot.scheduled_task_delivery ENABLE ROW LEVEL SECURITY;
ALTER TABLE tombot.scheduled_task_delivery FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON tombot.scheduled_task_delivery TO tombot_tenant
 USING(user_id=nullif(current_setting('tombot.user_id',true),'')) WITH CHECK(user_id=nullif(current_setting('tombot.user_id',true),''));
GRANT SELECT,INSERT,UPDATE,DELETE ON tombot.scheduled_task_delivery TO tombot_tenant;
CREATE POLICY dispatch_deliveries ON tombot.scheduled_task_delivery TO tombot_dispatch USING(true);
GRANT SELECT ON tombot.scheduled_task_delivery TO tombot_dispatch;

-- 已投递任务结果支持普通引用和表情；保留前序、同会话及完整状态约束。
CREATE OR REPLACE FUNCTION tombot.check_message_reply() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF NEW.reply_to_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM tombot.messages target
        WHERE target.user_id=NEW.user_id AND target.conversation_id=NEW.conversation_id
            AND target.id=NEW.reply_to_id AND target.sequence<NEW.sequence AND target.status='complete'
            AND (target.sender_kind IN ('user','bot') OR
                (target.sender_kind='system' AND target.blocks @> '[{"kind":"text","task_result":true}]'::jsonb))
    ) THEN
        RAISE EXCEPTION '回复目标必须是当前会话中已有的完整消息' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
