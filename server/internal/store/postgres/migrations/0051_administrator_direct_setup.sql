-- 管理员改为创建时直接设置口令，并一次性返回动态验证器密钥；不再存在"待激活管理员"。
-- 管理员不是被邀请的外部账号，而是由已经登录的管理员或首位部署者当面开立，
-- 因此不需要一次性激活材料，也不需要单独的登记步骤。

-- 先移除激活材料表及其授权，避免留下永不再用的写入路径。
DROP TABLE tombot.administrator_activation_mfa;
DROP TABLE tombot.administrator_activation_tokens;

ALTER TABLE tombot.administrators DROP CONSTRAINT administrators_status_check;
-- 历史待激活管理员已无法完成激活，统一停用，由管理员按新流程重建，避免出现无法登录的账号。
UPDATE tombot.administrators SET status = 'disabled' WHERE status = 'pending_activation';
ALTER TABLE tombot.administrators ADD CONSTRAINT administrators_status_check CHECK (status IN ('active', 'disabled'));
