-- 每个节点仅保存最近一次维护身份；完成记录阻止旧签名重新开启维护，不改写用户电脑。
CREATE TABLE tombot.node_upgrades (
    node_id text PRIMARY KEY REFERENCES tombot.node_connections(node_id),
    operation_id text NOT NULL CHECK (operation_id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    target_image_id text NOT NULL,
    is_finished boolean NOT NULL DEFAULT false
);
GRANT SELECT, INSERT, UPDATE (operation_id, target_image_id, is_finished) ON tombot.node_upgrades TO tombot_control;
