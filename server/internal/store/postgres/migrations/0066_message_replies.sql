-- 引用绑定同一用户、同一会话内的原消息；历史正文不可更新，因此无需复制原文。
ALTER TABLE tombot.messages
    ADD COLUMN reply_to_id text,
    ADD CONSTRAINT messages_conversation_identity UNIQUE (user_id,conversation_id,id),
    ADD CONSTRAINT messages_reply_target FOREIGN KEY (user_id,conversation_id,reply_to_id)
        REFERENCES tombot.messages(user_id,conversation_id,id),
    ADD CONSTRAINT messages_reply_not_self CHECK (reply_to_id IS NULL OR reply_to_id<>id);

CREATE FUNCTION tombot.check_message_reply() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
    IF NEW.reply_to_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM tombot.messages target
        WHERE target.user_id=NEW.user_id AND target.conversation_id=NEW.conversation_id
            AND target.id=NEW.reply_to_id AND target.sequence<NEW.sequence
            AND target.status='complete' AND target.sender_kind IN ('user','bot')
    ) THEN
        RAISE EXCEPTION '回复目标必须是当前会话中已有的完整消息' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$function$;
REVOKE ALL ON FUNCTION tombot.check_message_reply() FROM PUBLIC;
CREATE TRIGGER messages_reply_valid BEFORE INSERT OR UPDATE ON tombot.messages
    FOR EACH ROW EXECUTE FUNCTION tombot.check_message_reply();
