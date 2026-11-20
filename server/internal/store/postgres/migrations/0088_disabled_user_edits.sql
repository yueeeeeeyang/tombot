-- 新表从空开始，无业务回填。电脑编辑占用原电脑直至目标代次通过验收。
CREATE TABLE tombot.computer_edits (
    user_id text PRIMARY KEY, computer_id text NOT NULL, edit_id text NOT NULL UNIQUE,
    source_image_id text NOT NULL REFERENCES tombot.computer_images(id),
    target_image_id text NOT NULL REFERENCES tombot.computer_images(id),
    source_spec jsonb NOT NULL, target_spec jsonb NOT NULL, source_container text NOT NULL,
    stage text NOT NULL CHECK(stage IN ('checking','copying','installing','succeeded')),
    copied bigint NOT NULL DEFAULT 0 CHECK(copied>=0),
    attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0),
    failure text, next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    lease_until timestamptz,
    FOREIGN KEY(user_id,computer_id) REFERENCES tombot.computers(user_id,id)
);
CREATE INDEX computer_edits_pending ON tombot.computer_edits(next_attempt_at,user_id) WHERE stage<>'succeeded';
ALTER TABLE tombot.computer_edits ENABLE ROW LEVEL SECURITY;
ALTER TABLE tombot.computer_edits FORCE ROW LEVEL SECURITY;
CREATE POLICY control_computer_edits ON tombot.computer_edits TO tombot_control USING(true) WITH CHECK(true);
GRANT SELECT, INSERT, UPDATE ON tombot.computer_edits TO tombot_control;
CREATE POLICY control_disabled_avatar ON tombot.user_avatars TO tombot_control
 USING(EXISTS(SELECT 1 FROM tombot.users u WHERE u.id=user_id AND u.status='disabled'))
 WITH CHECK(EXISTS(SELECT 1 FROM tombot.users u WHERE u.id=user_id AND u.status='disabled'));
GRANT SELECT(user_id), INSERT(user_id,content), UPDATE(content,updated_at), DELETE ON tombot.user_avatars TO tombot_control;

-- 在切换完成前维持禁用，避免迁移窗口被另一个入口提前开放。
CREATE FUNCTION tombot.guard_user_edit_activation() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM tombot.computer_edits WHERE user_id=NEW.id AND stage<>'succeeded') THEN
    RAISE EXCEPTION '电脑配置迁移尚未完成' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION tombot.guard_user_edit_activation() FROM PUBLIC;
CREATE TRIGGER guard_user_edit_activation BEFORE UPDATE OF status ON tombot.users FOR EACH ROW
 WHEN (OLD.status='disabled' AND NEW.status<>OLD.status) EXECUTE FUNCTION tombot.guard_user_edit_activation();
