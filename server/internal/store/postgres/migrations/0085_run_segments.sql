-- 分段使用同一个可取消的运行，分别记录模型输入与操作回执。已有记录全部属于首段。
ALTER TABLE tombot.runs ADD COLUMN segment bigint NOT NULL DEFAULT 0 CHECK(segment>=0),
 ADD COLUMN handoff text NOT NULL DEFAULT '' CHECK(octet_length(handoff)<=16384);
ALTER TABLE tombot.run_operations ADD COLUMN segment bigint NOT NULL DEFAULT 0 CHECK(segment>=0),
 DROP CONSTRAINT run_operations_user_id_run_id_step_key_key,
 ADD UNIQUE(user_id,run_id,segment,step_key);
ALTER TABLE tombot.run_input_batches ADD COLUMN segment bigint NOT NULL DEFAULT 0 CHECK(segment>=0),
 DROP CONSTRAINT run_input_batches_pkey, ADD PRIMARY KEY(user_id,run_id,segment,step_key);
-- 使用唯一索引识别往复重复的阶段，无需加载历史工具输出。
CREATE TABLE tombot.run_checkpoints (
 user_id text NOT NULL,
 run_id text NOT NULL,
 segment bigint NOT NULL CHECK(segment>=0),
 evidence_digest bytea NOT NULL CHECK(octet_length(evidence_digest)=32),
 PRIMARY KEY(user_id,run_id,segment),
 UNIQUE(user_id,run_id,evidence_digest),
 FOREIGN KEY(user_id,run_id) REFERENCES tombot.runs(user_id,id)
);
ALTER TABLE tombot.run_checkpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE tombot.run_checkpoints FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON tombot.run_checkpoints TO tombot_tenant
 USING(user_id=nullif(current_setting('tombot.user_id',true),''))
 WITH CHECK(user_id=nullif(current_setting('tombot.user_id',true),''));
GRANT SELECT,INSERT ON tombot.run_checkpoints TO tombot_tenant;
