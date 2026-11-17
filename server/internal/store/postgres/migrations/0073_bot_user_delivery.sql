-- 直接交付由持久私信控制；旧请求保持返回发起猫咪的语义，不回填正文。
ALTER TABLE tombot.bot_private_messages ADD COLUMN deliver_to_user boolean NOT NULL DEFAULT false;

-- 猫咪主动发送给用户的正文允许引用，私信内部输入和展示提示仍禁止公开引用。
CREATE OR REPLACE FUNCTION tombot.check_message_reply() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,tombot,pg_temp AS $function$
BEGIN
 IF NEW.reply_to_id IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM tombot.messages target
  WHERE target.user_id=NEW.user_id AND target.conversation_id=NEW.conversation_id
   AND target.id=NEW.reply_to_id AND target.sequence<NEW.sequence AND target.status='complete'
   AND target.visible_to_bot_id IS NULL
   AND (target.sender_kind IN ('user','bot') OR (target.sender_kind='system' AND
    (target.blocks @> '[{"kind":"text","task_result":true}]'::jsonb OR
     target.blocks @> '[{"kind":"text","user_message":true}]'::jsonb)))
 ) THEN
  RAISE EXCEPTION '回复目标必须是当前会话中已有的完整消息' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END
$function$;
