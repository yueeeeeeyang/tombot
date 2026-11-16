-- 用户头像不进入业务电脑或共享文件系统，只允许身份连接读写。
-- 新表为空，无历史回填；列权限变更只需短暂元数据锁，迁移失败整体回滚。
-- 已提交后优先前向修复；降级须恢复升级前一致备份及对应程序，禁止修改本脚本或账本。
GRANT UPDATE (display_name) ON tombot.users TO tombot_identity;

CREATE TABLE tombot.user_avatars (
    user_id text PRIMARY KEY REFERENCES tombot.users(id),
    content bytea NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 2097152),
    updated_at timestamptz NOT NULL DEFAULT transaction_timestamp()
);
ALTER TABLE tombot.user_avatars ENABLE ROW LEVEL SECURITY;
ALTER TABLE tombot.user_avatars FORCE ROW LEVEL SECURITY;
CREATE POLICY identity_user_avatar ON tombot.user_avatars TO tombot_identity USING (true) WITH CHECK (true);
GRANT SELECT, INSERT, UPDATE ON tombot.user_avatars TO tombot_identity;
