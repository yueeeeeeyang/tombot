-- 桌面身份属于最终数据模型，不为旧机器人推断或补造身份。
DO $empty_catalog$
BEGIN
    IF EXISTS(SELECT 1 FROM tombot.bots) THEN
        RAISE EXCEPTION '桌面模型初始化需要空机器人目录，不支持旧数据转换' USING ERRCODE='55000';
    END IF;
END
$empty_catalog$;

ALTER TABLE tombot.bots ADD COLUMN desktop_id text NOT NULL CHECK(desktop_id ~ '^[0-9A-HJKMNP-TV-Z]{26}$');
ALTER TABLE tombot.bots ADD CONSTRAINT bots_desktop_binding UNIQUE(user_id,id,desktop_id);

-- 编号只前进且不回收，避免旧桌面目录和迟到控制请求获得另一个机器人的身份。
CREATE TABLE tombot.desktop_identity_heads (
    user_id text PRIMARY KEY,
    computer_id text NOT NULL,
    shared_gid integer NOT NULL CHECK(shared_gid=20000),
    last_display_number integer NOT NULL DEFAULT 0 CHECK(last_display_number BETWEEN 0 AND 4095),
    UNIQUE(user_id,computer_id,shared_gid),
    FOREIGN KEY(user_id,computer_id) REFERENCES tombot.computers(user_id,id)
);

CREATE TABLE tombot.bot_desktops (
    user_id text NOT NULL,
    id text NOT NULL CHECK(id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    bot_id text NOT NULL,
    computer_id text NOT NULL,
    linux_uid integer NOT NULL,
    shared_gid integer NOT NULL,
    display_number integer NOT NULL CHECK(display_number BETWEEN 1 AND 4095),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(user_id,id),
    UNIQUE(user_id,bot_id),
    UNIQUE(user_id,id,bot_id),
    UNIQUE(user_id,computer_id,linux_uid),
    UNIQUE(user_id,computer_id,display_number),
    CHECK(linux_uid=100000+display_number),
    FOREIGN KEY(user_id,computer_id,shared_gid) REFERENCES tombot.desktop_identity_heads(user_id,computer_id,shared_gid),
    FOREIGN KEY(user_id,bot_id,id) REFERENCES tombot.bots(user_id,id,desktop_id) DEFERRABLE INITIALLY DEFERRED
);
ALTER TABLE tombot.bots ADD CONSTRAINT bots_desktop_owner
    FOREIGN KEY(user_id,desktop_id,id) REFERENCES tombot.bot_desktops(user_id,id,bot_id) DEFERRABLE INITIALLY DEFERRED;

CREATE FUNCTION tombot.protect_desktop_identity() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF TG_OP='DELETE' OR NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION '机器人桌面身份不可改写或回收' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.protect_desktop_identity() FROM PUBLIC;
CREATE TRIGGER bot_desktops_fixed BEFORE UPDATE OR DELETE ON tombot.bot_desktops FOR EACH ROW EXECUTE FUNCTION tombot.protect_desktop_identity();

CREATE FUNCTION tombot.protect_desktop_identity_head() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF TG_OP='DELETE' OR (NEW.user_id,NEW.computer_id,NEW.shared_gid) IS DISTINCT FROM (OLD.user_id,OLD.computer_id,OLD.shared_gid)
       OR NEW.last_display_number<OLD.last_display_number THEN
        RAISE EXCEPTION '桌面身份分配记录不可更换或倒退' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.protect_desktop_identity_head() FROM PUBLIC;
CREATE TRIGGER desktop_identity_heads_fixed BEFORE UPDATE OR DELETE ON tombot.desktop_identity_heads FOR EACH ROW EXECUTE FUNCTION tombot.protect_desktop_identity_head();

CREATE FUNCTION tombot.protect_bot_desktop_binding() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF NEW.desktop_id IS DISTINCT FROM OLD.desktop_id THEN
        RAISE EXCEPTION '机器人所属桌面不可更换' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.protect_bot_desktop_binding() FROM PUBLIC;
CREATE TRIGGER bots_desktop_fixed BEFORE UPDATE ON tombot.bots FOR EACH ROW EXECUTE FUNCTION tombot.protect_bot_desktop_binding();

DO $policies$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['desktop_identity_heads','bot_desktops'] LOOP
        EXECUTE format('ALTER TABLE tombot.%I ENABLE ROW LEVEL SECURITY',table_name);
        EXECUTE format('ALTER TABLE tombot.%I FORCE ROW LEVEL SECURITY',table_name);
        EXECUTE format('CREATE POLICY tenant_scope ON tombot.%I TO tombot_tenant USING (user_id=nullif(current_setting(''tombot.user_id'',true),'''')) WITH CHECK (user_id=nullif(current_setting(''tombot.user_id'',true),''''))',table_name);
        EXECUTE format('CREATE POLICY control_desktop_catalog ON tombot.%I TO tombot_control USING(true)',table_name);
    END LOOP;
END
$policies$;
GRANT SELECT,INSERT ON tombot.desktop_identity_heads,tombot.bot_desktops TO tombot_tenant;
GRANT UPDATE(last_display_number) ON tombot.desktop_identity_heads TO tombot_tenant;
GRANT SELECT ON tombot.desktop_identity_heads,tombot.bot_desktops TO tombot_control;
