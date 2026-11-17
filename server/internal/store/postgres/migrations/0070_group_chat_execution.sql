-- 每条群消息有独立回合，成员运行复用已有租约、用量和操作账本。
DROP INDEX tombot.turns_active_conversation;
CREATE UNIQUE INDEX turns_active_conversation ON tombot.turns(user_id,conversation_id)
 WHERE status IN ('queued','running','canceling') AND source<>'group';
CREATE TABLE tombot.group_deliveries (
 user_id text NOT NULL,
 run_id text NOT NULL,
 bot_id text NOT NULL,
 conversation_id text NOT NULL,
 trigger_message_id text NOT NULL,
 depth integer NOT NULL CHECK(depth BETWEEN 0 AND 4),
 roster jsonb NOT NULL CHECK(jsonb_typeof(roster)='array' AND jsonb_array_length(roster) BETWEEN 1 AND 32),
 PRIMARY KEY(user_id,run_id),
 UNIQUE(user_id,trigger_message_id,bot_id),
 FOREIGN KEY(user_id,run_id,bot_id) REFERENCES tombot.runs(user_id,id,bot_id),
 FOREIGN KEY(user_id,conversation_id,trigger_message_id) REFERENCES tombot.messages(user_id,conversation_id,id)
);
ALTER TABLE tombot.group_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE tombot.group_deliveries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON tombot.group_deliveries TO tombot_tenant
 USING(user_id=nullif(current_setting('tombot.user_id',true),'')) WITH CHECK(user_id=nullif(current_setting('tombot.user_id',true),''));
GRANT SELECT,INSERT ON tombot.group_deliveries TO tombot_tenant;
