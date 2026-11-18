-- 已有电脑先停止签发旧实例会话，再凭节点的同代次探针回执提交镜像切换。
ALTER TABLE tombot.node_connections ADD COLUMN supports_computer_upgrades boolean NOT NULL DEFAULT false;
GRANT UPDATE (supports_computer_upgrades) ON tombot.node_connections TO tombot_control;
GRANT SELECT (node_id,image_id,supports_computer_upgrades) ON tombot.node_connections TO tombot_tenant;
GRANT SELECT (node_id,is_finished) ON tombot.node_upgrades TO tombot_tenant;

CREATE TABLE tombot.computer_image_upgrades (
    user_id text PRIMARY KEY,
    computer_id text NOT NULL,
    node_id text NOT NULL REFERENCES tombot.computer_nodes(id),
    source_image_id text NOT NULL REFERENCES tombot.computer_images(id),
    target_image_id text NOT NULL REFERENCES tombot.computer_images(id),
    source_generation bigint NOT NULL CHECK (source_generation > 0),
    source_container_id text NOT NULL CHECK (source_container_id ~ '^[0-9a-f]{64}$'),
    source_specification jsonb NOT NULL CHECK (jsonb_typeof(source_specification) = 'object'),
    target_specification jsonb NOT NULL CHECK (jsonb_typeof(target_specification) = 'object'),
    state text NOT NULL CHECK (state IN ('draining','switching','succeeded','quarantined')),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    lease_expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (user_id,computer_id) REFERENCES tombot.computers(user_id,id),
    CHECK (source_image_id <> target_image_id)
);
CREATE INDEX computer_image_upgrades_pending ON tombot.computer_image_upgrades(next_attempt_at, user_id)
    WHERE state IN ('draining','switching');
CREATE INDEX computer_image_upgrades_node_pending ON tombot.computer_image_upgrades(node_id)
    WHERE state IN ('draining','switching','quarantined');
CREATE INDEX computers_node_image ON tombot.computers(node_id,image_id,user_id) WHERE status='ready';

ALTER TABLE tombot.computer_image_upgrades ENABLE ROW LEVEL SECURITY;
ALTER TABLE tombot.computer_image_upgrades FORCE ROW LEVEL SECURITY;
CREATE POLICY control_computer_image_upgrades ON tombot.computer_image_upgrades TO tombot_control USING (true) WITH CHECK (true);
CREATE POLICY tenant_read_computer_image_upgrades ON tombot.computer_image_upgrades FOR SELECT TO tombot_tenant
    USING (user_id = nullif(current_setting('tombot.user_id', true), ''));
GRANT SELECT, INSERT, UPDATE ON tombot.computer_image_upgrades TO tombot_control;
GRANT SELECT (user_id,computer_id,node_id,source_generation,source_container_id,state) ON tombot.computer_image_upgrades TO tombot_tenant;

-- 迁移与聊天准入获取同一个锁，控制连接无需读取或改变执行令牌。
CREATE POLICY control_computer_upgrade_lock ON tombot.computer_execution_heads TO tombot_control USING (true) WITH CHECK (true);
GRANT SELECT (user_id), INSERT (user_id,computer_id), UPDATE (computer_id) ON tombot.computer_execution_heads TO tombot_control;
