-- 控制心跳不代表能力就绪；旧就绪声明没有租约证据，必须重新验收。
ALTER TABLE tombot.computer_nodes
    ADD COLUMN runtime_id text,
    ADD COLUMN control_sequence bigint NOT NULL DEFAULT 0,
    ADD COLUMN control_expires_at timestamptz;
UPDATE tombot.computer_nodes SET status='bootstrapping' WHERE status='ready';
ALTER TABLE tombot.computer_nodes ADD CONSTRAINT computer_node_control_lease_valid CHECK (
    (runtime_id IS NULL AND control_sequence=0 AND control_expires_at IS NULL AND status<>'ready') OR
    (runtime_id IS NOT NULL AND runtime_id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$' AND control_sequence>0 AND last_seen_at IS NOT NULL
        AND control_expires_at IS NOT NULL AND control_expires_at>last_seen_at)
);

-- 查询和准入共用有效状态规则；排空不因在线与否解除，过期租约不能支持 ready。
CREATE FUNCTION tombot.computer_node_status(stored_status text, runtime_id text, expires_at timestamptz)
RETURNS text LANGUAGE sql VOLATILE SET search_path=pg_catalog,tombot,pg_temp AS $function$
    SELECT CASE WHEN stored_status='draining' THEN 'draining'
        WHEN runtime_id IS NOT NULL AND expires_at<=clock_timestamp() THEN 'offline'
        ELSE stored_status END
$function$;
REVOKE ALL ON FUNCTION tombot.computer_node_status(text,text,timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tombot.computer_node_status(text,text,timestamptz) TO tombot_control;

-- 先取得行锁再检查时钟，避免 UPDATE 在锁等待前判断有效、等待结束后却用过期租约准入。
CREATE FUNCTION tombot.reserve_computer_node(requested_node text) RETURNS SETOF text
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
DECLARE node tombot.computer_nodes%ROWTYPE;
BEGIN
    SELECT * INTO node FROM tombot.computer_nodes WHERE id=requested_node FOR UPDATE;
    IF NOT FOUND THEN RETURN; END IF;
    IF tombot.computer_node_status(node.status,node.runtime_id,node.control_expires_at)='ready' AND node.reserved<node.capacity THEN
        UPDATE tombot.computer_nodes SET reserved=reserved+1 WHERE id=requested_node;
        RETURN NEXT requested_node;
    END IF;
END
$function$;
REVOKE ALL ON FUNCTION tombot.reserve_computer_node(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tombot.reserve_computer_node(text) TO tombot_control;

-- 不允许同代次换进程、倒退序号、删除绑定或以重复心跳延长租约。
CREATE FUNCTION tombot.protect_computer_node_control_lease() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF NEW.generation<OLD.generation THEN
        RAISE EXCEPTION '节点控制代次不可倒退' USING ERRCODE='23514';
    END IF;
    IF OLD.runtime_id IS NOT NULL THEN
        IF NEW.runtime_id IS NULL OR
            (NEW.generation=OLD.generation AND (NEW.runtime_id IS DISTINCT FROM OLD.runtime_id OR NEW.control_sequence<OLD.control_sequence)) OR
            (NEW.generation>OLD.generation AND NEW.runtime_id=OLD.runtime_id) THEN
            RAISE EXCEPTION '节点控制实例或序号不可复用' USING ERRCODE='23514';
        END IF;
        IF NEW.generation=OLD.generation AND NEW.control_sequence=OLD.control_sequence AND
            (NEW.last_seen_at IS DISTINCT FROM OLD.last_seen_at OR NEW.control_expires_at IS DISTINCT FROM OLD.control_expires_at) THEN
            RAISE EXCEPTION '重复节点心跳不可修改期限' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.protect_computer_node_control_lease() FROM PUBLIC;
CREATE TRIGGER computer_node_control_lease_fixed BEFORE UPDATE ON tombot.computer_nodes
    FOR EACH ROW EXECUTE FUNCTION tombot.protect_computer_node_control_lease();
