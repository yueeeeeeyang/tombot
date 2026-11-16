-- 执行中的补充消息有独立幂等键；模型边界固定输入批次，恢复不能改变已执行步骤。
-- 新表为空，无历史数据回填；新增列只短暂锁定运行表。失败时由迁移事务整体回滚。
ALTER TABLE tombot.runs ADD COLUMN context_sequence bigint CHECK (context_sequence>=0);
ALTER TABLE tombot.runs ADD COLUMN input_sequence bigint NOT NULL DEFAULT 0 CHECK (input_sequence>=0);
ALTER TABLE tombot.runs ADD COLUMN continuation_after bigint NOT NULL DEFAULT 0 CHECK (continuation_after>=0);
ALTER TABLE tombot.runs ADD COLUMN progress_summary text NOT NULL DEFAULT '' CHECK (char_length(progress_summary)<=60);
CREATE TABLE tombot.run_inputs (
    user_id text NOT NULL,
    client_request_id text NOT NULL CHECK (client_request_id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    request_digest bytea NOT NULL CHECK (octet_length(request_digest)=32),
    run_id text NOT NULL,
    turn_id text NOT NULL,
    conversation_id text NOT NULL,
    message_id text NOT NULL,
    PRIMARY KEY (user_id,client_request_id),
    FOREIGN KEY (user_id,run_id,turn_id,conversation_id) REFERENCES tombot.runs(user_id,id,turn_id,conversation_id),
    FOREIGN KEY (user_id,message_id) REFERENCES tombot.messages(user_id,id)
);
CREATE INDEX run_inputs_turn ON tombot.run_inputs(user_id,turn_id,message_id);
CREATE TABLE tombot.run_input_batches (
    user_id text NOT NULL,
    run_id text NOT NULL,
    step_key text NOT NULL CHECK (step_key ~ '^model\.[1-9][0-9]{0,2}$'),
    step integer NOT NULL CHECK (step BETWEEN 1 AND 64),
    message_offset integer NOT NULL CHECK (message_offset>=0),
    messages jsonb NOT NULL CHECK (jsonb_typeof(messages)='array' AND octet_length(messages::text)<=1048576),
    PRIMARY KEY (user_id,run_id,step_key),
    FOREIGN KEY (user_id,run_id) REFERENCES tombot.runs(user_id,id)
);
DO $policies$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['run_inputs','run_input_batches'] LOOP
        EXECUTE format('ALTER TABLE tombot.%I ENABLE ROW LEVEL SECURITY',table_name);
        EXECUTE format('ALTER TABLE tombot.%I FORCE ROW LEVEL SECURITY',table_name);
        EXECUTE format('CREATE POLICY tenant_scope ON tombot.%I TO tombot_tenant USING (user_id=nullif(current_setting(''tombot.user_id'',true),'''')) WITH CHECK (user_id=nullif(current_setting(''tombot.user_id'',true),''''))',table_name);
    END LOOP;
END
$policies$;
GRANT SELECT,INSERT ON tombot.run_inputs,tombot.run_input_batches TO tombot_tenant;
