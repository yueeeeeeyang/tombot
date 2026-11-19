-- 只登记从完整归档重新验证出的清单。历史发布 ID、节点任务 ID 与审计继续保留。
CREATE TABLE tombot.node_release_archives (
    artifact text PRIMARY KEY CHECK (artifact ~ '^[0-9a-f]{64}$'),
    manifest jsonb NOT NULL CHECK (jsonb_typeof(manifest) = 'object')
);
GRANT SELECT, INSERT ON tombot.node_release_archives TO tombot_control;
-- 旧协议不能领取自包含包，清理旧心跳并结束尚未完成的旧任务，以便管理员重新下发。
DELETE FROM tombot.node_update_agents;
UPDATE tombot.node_update_jobs SET state='failed',updated_at=clock_timestamp() WHERE state IN ('queued','running');
