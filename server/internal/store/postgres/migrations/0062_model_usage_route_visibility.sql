-- 用量校验以函数所有者身份执行；一键安装的迁移角色不绕过强制行级隔离。
-- 只允许该身份读取当前事务用户的路由，仍由原触发器核对运行、候选、额度和价格。
-- 仅更新策略元数据，不扫描或改写业务数据；锁等待受迁移器时限约束。
-- 回滚须恢复升级前数据库和程序，不能删除迁移账本或修改历史脚本。
DO $policy$
DECLARE verifier_owner name;
BEGIN
    SELECT pg_get_userbyid(proowner) INTO STRICT verifier_owner
        FROM pg_proc WHERE oid='tombot.check_usage_binding()'::regprocedure;
    EXECUTE format('CREATE POLICY model_routes_usage_verifier ON tombot.model_routes
        FOR SELECT TO %I USING(user_id=nullif(current_setting(''tombot.user_id'',true),''''))', verifier_owner);
END
$policy$;
