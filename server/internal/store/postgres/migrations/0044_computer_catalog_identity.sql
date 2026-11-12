-- 单宿主节点通过部署配置的受保护 Unix 通道访问，不再保存未使用的网络端点。
ALTER TABLE tombot.computer_nodes DROP COLUMN endpoint;
-- 停用或排空保留历史身份，运行角色不能删除后重建同 ID 来绕过版本保护。
REVOKE DELETE ON tombot.computer_nodes, tombot.computer_images FROM tombot_control;

-- 镜像换版必须登记新的 ID；不能修改已有电脑所引用的版本或节点归属。
CREATE FUNCTION tombot.protect_computer_node_identity() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id THEN
        RAISE EXCEPTION '节点标识不可改写' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.protect_computer_node_identity() FROM PUBLIC;
CREATE TRIGGER computer_node_identity_fixed BEFORE UPDATE ON tombot.computer_nodes
    FOR EACH ROW EXECUTE FUNCTION tombot.protect_computer_node_identity();

CREATE FUNCTION tombot.protect_computer_image_identity() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.reference IS DISTINCT FROM OLD.reference THEN
        RAISE EXCEPTION '镜像版本标识与引用不可改写' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.protect_computer_image_identity() FROM PUBLIC;
CREATE TRIGGER computer_image_identity_fixed BEFORE UPDATE ON tombot.computer_images
    FOR EACH ROW EXECUTE FUNCTION tombot.protect_computer_image_identity();
