-- 新数据库身份约束的前向补充：账户名使用与服务一致的规范，验证材料不能关联其他账户的激活记录。
ALTER TABLE tombot.users ADD CONSTRAINT users_username_characters CHECK (username ~ '^[a-z0-9_.-]{3,64}$');
ALTER TABLE tombot.activation_tokens ADD CONSTRAINT activation_tokens_user_id_id UNIQUE (user_id, id);
ALTER TABLE tombot.activation_mfa
    DROP CONSTRAINT activation_mfa_activation_id_fkey,
    DROP CONSTRAINT activation_mfa_user_id_fkey,
    ADD CONSTRAINT activation_mfa_owner FOREIGN KEY (user_id, activation_id) REFERENCES tombot.activation_tokens(user_id, id);
CREATE INDEX users_active_administrators ON tombot.users(id) WHERE role='admin' AND status='active';

-- 并发降权或停用必须串行判断最后一名有效管理员，避免两个请求各自看到对方而同时成功。
-- 控制服务在修改前先取得相同事务锁；触发器同时防止其他内部调用遗漏保护。
CREATE FUNCTION tombot.protect_last_administrator() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF OLD.role='admin' AND OLD.status='active' THEN
        IF TG_OP='UPDATE' AND NEW.role='admin' AND NEW.status='active' THEN
            RETURN NEW;
        END IF;
        PERFORM pg_advisory_xact_lock(hashtextextended('tombot.active_administrators',0));
        IF NOT EXISTS (SELECT 1 FROM tombot.users WHERE role='admin' AND status='active' AND id<>OLD.id) THEN
            RAISE EXCEPTION '不能移除最后一名有效管理员' USING ERRCODE='23514';
        END IF;
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.protect_last_administrator() FROM PUBLIC;
CREATE TRIGGER users_last_administrator BEFORE UPDATE OF role,status OR DELETE ON tombot.users
    FOR EACH ROW EXECUTE FUNCTION tombot.protect_last_administrator();

-- 空库初始化及当前小型身份表在有界迁移事务中完成；约束检查需要扫描用户与激活表并取得结构锁。
-- 执行器设置锁等待与语句时限，失败整体回滚；正式大表升级需先评估行数并新增分步验证脚本。
