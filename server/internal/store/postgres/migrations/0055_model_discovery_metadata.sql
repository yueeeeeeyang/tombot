-- 保存供应商元数据及所选思考级别；不猜测旧模型能力，不改写历史策略快照。
-- 新列使用常量默认值，增加约束时扫描小型模型目录并短暂持有结构锁；迁移失败整体回滚。
-- 降级须恢复升级前数据库及二进制，或新增前向修复；禁止修改已应用脚本。
ALTER TABLE tombot.models
 ADD COLUMN metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
 ADD COLUMN reasoning_effort text NOT NULL DEFAULT '',
 ADD CONSTRAINT models_metadata_object CHECK(jsonb_typeof(metadata)='object' AND octet_length(metadata::text)<=8192),
 ADD CONSTRAINT models_reasoning_effort CHECK(reasoning_effort='' OR (
  reasoning_effort ~ '^[a-z][a-z0-9_-]{0,31}$' AND
  coalesce((metadata->'effort'->'supported_levels') ? reasoning_effort,false)));
