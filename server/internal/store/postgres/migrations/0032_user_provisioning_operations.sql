-- 同步开户由持久操作驱动。请求等待与节点执行分离，不持有数据库事务等待容器。
ALTER TABLE tombot.user_creation_operations
    ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    ADD COLUMN reconcile_count integer NOT NULL DEFAULT 0 CHECK (reconcile_count >= 0),
    ADD COLUMN capacity_reserved boolean NOT NULL DEFAULT true,
    ADD COLUMN request_id text;
UPDATE tombot.user_creation_operations SET request_id=id WHERE request_id IS NULL;
ALTER TABLE tombot.user_creation_operations
    ALTER COLUMN request_id SET NOT NULL,
    ADD CONSTRAINT user_creation_request_id CHECK (length(request_id) BETWEEN 1 AND 120),
    ADD CONSTRAINT user_creation_capacity CHECK (status <> 'failed' OR NOT capacity_reserved);
CREATE INDEX user_creation_claim ON tombot.user_creation_operations(next_attempt_at,created_at,id)
    WHERE status IN ('pending','running','reconciling','compensating');
ALTER TABLE tombot.computers ADD COLUMN ready_at timestamptz;

-- 当前为空库或小型操作表的有界事务升级。更新账本行和建索引需要结构锁；失败整体回滚。
-- 以后大表升级先评估锁等待与行数，修改本版本只能通过新增更高版本脚本实施。
