-- 配置版本仅在管理者修改名称、准入容量或接单设置时递增，心跳不制造编辑冲突。
ALTER TABLE tombot.computer_nodes
    ADD COLUMN configuration_version bigint NOT NULL DEFAULT 1 CHECK (configuration_version > 0);

-- 保留节点返回的受限错误分类，不保存命令、环境变量或原始远端响应。
ALTER TABLE tombot.node_verifications
    ADD COLUMN node_error_code text CHECK (node_error_code ~ '^[a-z][a-z0-9_]{0,63}$');
