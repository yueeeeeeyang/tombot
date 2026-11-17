-- 私信与回复共用持久运行；正文只留在租户表，调度器仅能读取无正文的投递索引。
ALTER TABLE tombot.runs ADD COLUMN bot_directory_context text CHECK(octet_length(bot_directory_context)<=32768);
ALTER TABLE tombot.runs ADD COLUMN private_message_attempts integer NOT NULL DEFAULT 0 CHECK(private_message_attempts>=0);
ALTER TABLE tombot.messages ADD COLUMN visible_to_bot_id text;
ALTER TABLE tombot.messages ADD CONSTRAINT messages_private_recipient FOREIGN KEY(user_id,visible_to_bot_id) REFERENCES tombot.bots(user_id,id);
ALTER TABLE tombot.messages ADD CONSTRAINT messages_private_input CHECK(visible_to_bot_id IS NULL OR (sender_kind='system' AND blocks->0 ? 'bot_message'));
CREATE TABLE tombot.bot_private_messages (
 user_id text NOT NULL,
 id text NOT NULL,
 chain_id text NOT NULL,
 parent_id text,
 sender_bot_id text NOT NULL,
 recipient_bot_id text NOT NULL,
 source_run_id text NOT NULL,
 source_conversation_id text NOT NULL,
 source_path jsonb NOT NULL CHECK(jsonb_typeof(source_path)='array' AND jsonb_array_length(source_path) BETWEEN 1 AND 4),
 conversation_id text NOT NULL,
 request_message_id text NOT NULL,
 status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','waiting','ready','delivered','discarded')),
 response_text text NOT NULL DEFAULT '' CHECK(octet_length(response_text)<=65536),
 response_status text NOT NULL DEFAULT '' CHECK(response_status IN ('','succeeded','failed','canceled','unknown')),
 error_code text NOT NULL DEFAULT '',
 delivery_run_id text,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(user_id,id),
 FOREIGN KEY(user_id,id) REFERENCES tombot.run_operations(user_id,id),
 FOREIGN KEY(user_id,chain_id) REFERENCES tombot.runs(user_id,id),
 FOREIGN KEY(user_id,parent_id) REFERENCES tombot.bot_private_messages(user_id,id),
 FOREIGN KEY(user_id,source_run_id,sender_bot_id) REFERENCES tombot.runs(user_id,id,bot_id),
 FOREIGN KEY(user_id,recipient_bot_id) REFERENCES tombot.bots(user_id,id),
 FOREIGN KEY(user_id,source_conversation_id) REFERENCES tombot.conversations(user_id,id),
 FOREIGN KEY(user_id,conversation_id,request_message_id) REFERENCES tombot.messages(user_id,conversation_id,id),
 FOREIGN KEY(user_id,delivery_run_id) REFERENCES tombot.runs(user_id,id),
 CHECK(sender_bot_id<>recipient_bot_id)
);
CREATE INDEX bot_private_messages_chain ON tombot.bot_private_messages(user_id,chain_id);
CREATE INDEX bot_private_messages_parent ON tombot.bot_private_messages(user_id,parent_id) WHERE status IN ('pending','waiting','ready');
CREATE INDEX bot_private_messages_sender ON tombot.bot_private_messages(user_id,sender_bot_id) WHERE status IN ('pending','waiting','ready');
CREATE INDEX bot_private_messages_recipient ON tombot.bot_private_messages(user_id,recipient_bot_id) WHERE status='waiting';
CREATE INDEX jobs_bot_pending ON tombot.jobs(user_id,bot_id,status) WHERE status<>'finished';
CREATE TABLE tombot.bot_private_runs (
 user_id text NOT NULL,
 run_id text NOT NULL,
 chain_id text NOT NULL,
 response_to text,
 path jsonb NOT NULL CHECK(jsonb_typeof(path)='array' AND jsonb_array_length(path) BETWEEN 1 AND 5),
 trigger_message_id text NOT NULL,
 conversation_id text NOT NULL,
 PRIMARY KEY(user_id,run_id),
 FOREIGN KEY(user_id,run_id) REFERENCES tombot.runs(user_id,id),
 FOREIGN KEY(user_id,chain_id) REFERENCES tombot.runs(user_id,id),
 FOREIGN KEY(user_id,response_to) REFERENCES tombot.bot_private_messages(user_id,id),
 FOREIGN KEY(user_id,conversation_id,trigger_message_id) REFERENCES tombot.messages(user_id,conversation_id,id)
);
CREATE TABLE tombot.bot_private_delivery (
 user_id text NOT NULL,
 message_id text NOT NULL,
 bot_id text NOT NULL,
 conversation_id text NOT NULL,
 available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(user_id,message_id),
 FOREIGN KEY(user_id,message_id) REFERENCES tombot.bot_private_messages(user_id,id),
 FOREIGN KEY(user_id,bot_id) REFERENCES tombot.bots(user_id,id),
 FOREIGN KEY(user_id,conversation_id) REFERENCES tombot.conversations(user_id,id)
);
CREATE INDEX bot_private_delivery_queue ON tombot.bot_private_delivery(available_at,user_id,message_id);
DO $policies$
DECLARE table_name text;
BEGIN
 FOREACH table_name IN ARRAY ARRAY['bot_private_messages','bot_private_runs','bot_private_delivery'] LOOP
  EXECUTE format('ALTER TABLE tombot.%I ENABLE ROW LEVEL SECURITY',table_name);
  EXECUTE format('ALTER TABLE tombot.%I FORCE ROW LEVEL SECURITY',table_name);
  EXECUTE format('CREATE POLICY tenant_scope ON tombot.%I TO tombot_tenant USING (user_id=nullif(current_setting(''tombot.user_id'',true),'''')) WITH CHECK (user_id=nullif(current_setting(''tombot.user_id'',true),''''))',table_name);
 END LOOP;
END
$policies$;
GRANT SELECT,INSERT,UPDATE ON tombot.bot_private_messages TO tombot_tenant;
GRANT SELECT,INSERT ON tombot.bot_private_runs TO tombot_tenant;
GRANT SELECT,INSERT,UPDATE,DELETE ON tombot.bot_private_delivery TO tombot_tenant;
CREATE POLICY dispatch_private_delivery ON tombot.bot_private_delivery TO tombot_dispatch USING(true);
GRANT SELECT ON tombot.bot_private_delivery TO tombot_dispatch;

-- 私信只进入指定猫咪的执行上下文，禁止通过公开引用泄露内部原文。
CREATE OR REPLACE FUNCTION tombot.check_message_reply() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
 IF NEW.reply_to_id IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM tombot.messages target
  WHERE target.user_id=NEW.user_id AND target.conversation_id=NEW.conversation_id
   AND target.id=NEW.reply_to_id AND target.sequence<NEW.sequence AND target.status='complete'
   AND target.visible_to_bot_id IS NULL
   AND (target.sender_kind IN ('user','bot') OR (target.sender_kind='system' AND
    target.blocks @> '[{"kind":"text","task_result":true}]'::jsonb))
 ) THEN
  RAISE EXCEPTION '回复目标必须是当前会话中已有的完整消息' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END
$function$;
