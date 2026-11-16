-- 普通用户浏览器会话与桌面、管理员会话分别鉴权，复用用户撤销和改密失效机制。
-- 约束替换会短暂锁定会话表并校验现有行；升级前停单，超时整体回滚。
-- 回滚须同时恢复升级前数据库和程序，不删除迁移记录或改写历史脚本。
ALTER TABLE tombot.user_sessions DROP CONSTRAINT user_sessions_scope_check;
ALTER TABLE tombot.user_sessions DROP CONSTRAINT user_sessions_scope;
ALTER TABLE tombot.user_sessions ADD CONSTRAINT user_sessions_scope CHECK (scope IN ('desktop','web'));
