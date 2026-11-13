-- 管理员只执行管理动作，不分配用户电脑，也不登录桌面客户端。
-- 因此管理员的开户操作可以没有电脑记录；普通用户仍必须以真实电脑完成开户。
ALTER TABLE tombot.user_creation_operations
    ALTER COLUMN computer_id DROP NOT NULL;

-- 没有电脑的操作必然是管理员免资源开户，不能预留容量，也不能声称清理了资源。
ALTER TABLE tombot.user_creation_operations
    ADD CONSTRAINT user_creation_computer_or_reserved CHECK (computer_id IS NOT NULL OR NOT capacity_reserved);

-- 复合外键在任一列为 NULL 时不参与校验，因此"只有管理员能省略电脑"必须由触发器保证。
CREATE FUNCTION tombot.check_admin_without_computer() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF NEW.computer_id IS NULL AND NOT EXISTS (
        SELECT 1 FROM tombot.users WHERE users.id = NEW.user_id AND users.role = 'admin'
    ) THEN
        RAISE EXCEPTION '只有管理员账户可以没有电脑' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;

CREATE TRIGGER user_creation_admin_without_computer
    BEFORE INSERT OR UPDATE ON tombot.user_creation_operations
    FOR EACH ROW EXECUTE FUNCTION tombot.check_admin_without_computer();
