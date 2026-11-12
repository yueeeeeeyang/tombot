-- 节点验收使用独立临时电脑，不创建可登录的业务用户；每节点最多保留一项未清理操作。
CREATE TABLE tombot.node_verifications (
    node_id text PRIMARY KEY REFERENCES tombot.computer_nodes(id),
    image_id text NOT NULL REFERENCES tombot.computer_images(id),
    specification jsonb NOT NULL CHECK (jsonb_typeof(specification)='object'),
    runtime_id text NOT NULL,
    runtime_generation bigint NOT NULL CHECK (runtime_generation>0),
    is_valid boolean NOT NULL DEFAULT true,
    state text NOT NULL CHECK (state IN ('checking','cleaning','succeeded','failed','quarantined')),
    fencing_token bigint NOT NULL DEFAULT 0 CHECK (fencing_token>=0),
    lease_expires_at timestamptz,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts>=0),
    capabilities jsonb,
    verified_at timestamptz,
    error_code text,
    next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((verified_at IS NULL AND capabilities IS NULL) OR (verified_at IS NOT NULL AND capabilities IS NOT NULL AND jsonb_typeof(capabilities)='object'))
);
GRANT SELECT,INSERT,UPDATE ON tombot.node_verifications TO tombot_control;

ALTER TABLE tombot.computer_nodes
    ADD COLUMN verified_image_id text REFERENCES tombot.computer_images(id),
    ADD COLUMN capability_expires_at timestamptz;
UPDATE tombot.computer_nodes SET status='bootstrapping' WHERE status='ready';
ALTER TABLE tombot.computer_nodes ADD CONSTRAINT computer_node_capability_proof_valid CHECK (
    (verified_image_id IS NULL AND capability_expires_at IS NULL AND status<>'ready') OR
    (verified_image_id IS NOT NULL AND capability_expires_at IS NOT NULL)
);

-- 心跳中断后恢复也使正在执行的验收失效，即使节点进程未换代，旧检查也不能重新发布就绪。
CREATE FUNCTION tombot.invalidate_node_verification() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF NEW.runtime_id IS DISTINCT FROM OLD.runtime_id OR NEW.generation<>OLD.generation OR
        (NEW.control_sequence>OLD.control_sequence AND OLD.control_expires_at<=clock_timestamp()) THEN
        UPDATE tombot.node_verifications SET is_valid=false WHERE node_id=NEW.id;
        NEW.verified_image_id=NULL;
        NEW.capability_expires_at=NULL;
        IF NEW.status='ready' THEN NEW.status='bootstrapping'; END IF;
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.invalidate_node_verification() FROM PUBLIC;
CREATE TRIGGER node_verification_invalidated BEFORE UPDATE ON tombot.computer_nodes
    FOR EACH ROW EXECUTE FUNCTION tombot.invalidate_node_verification();

CREATE FUNCTION tombot.computer_node_status(stored_status text,runtime_id text,control_expires_at timestamptz,capability_expires_at timestamptz)
RETURNS text LANGUAGE sql VOLATILE SET search_path=pg_catalog,tombot,pg_temp AS $function$
    SELECT CASE WHEN stored_status='draining' THEN 'draining'
        WHEN runtime_id IS NOT NULL AND control_expires_at<=clock_timestamp() THEN 'offline'
        WHEN stored_status='ready' AND (capability_expires_at IS NULL OR capability_expires_at<=clock_timestamp()) THEN 'bootstrapping'
        ELSE stored_status END
$function$;
REVOKE ALL ON FUNCTION tombot.computer_node_status(text,text,timestamptz,timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tombot.computer_node_status(text,text,timestamptz,timestamptz) TO tombot_control;

-- 节点就绪仅支持实际通过验收的镜像；先锁行，再检查两种期限及容量。
CREATE FUNCTION tombot.reserve_computer_node(requested_node text,requested_image text) RETURNS SETOF text
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
DECLARE node tombot.computer_nodes%ROWTYPE;
BEGIN
    SELECT * INTO node FROM tombot.computer_nodes WHERE id=requested_node FOR UPDATE;
    IF NOT FOUND THEN RETURN; END IF;
    PERFORM 1 FROM tombot.computer_images WHERE id=requested_image AND is_enabled FOR SHARE;
    IF NOT FOUND THEN RETURN; END IF;
    IF tombot.computer_node_status(node.status,node.runtime_id,node.control_expires_at,node.capability_expires_at)='ready'
        AND node.verified_image_id=requested_image AND node.reserved<node.capacity THEN
        UPDATE tombot.computer_nodes SET reserved=reserved+1 WHERE id=requested_node;
        RETURN NEXT requested_node;
    END IF;
END
$function$;
REVOKE ALL ON FUNCTION tombot.reserve_computer_node(text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tombot.reserve_computer_node(text,text) TO tombot_control;
DROP FUNCTION tombot.reserve_computer_node(text);
DROP FUNCTION tombot.computer_node_status(text,text,timestamptz);
