-- 自定义头像独立存储；旧品种和工作记忆不变。
ALTER TABLE bots ADD COLUMN custom_avatar TEXT NOT NULL DEFAULT '';
-- 旧桌面猫咪也能直接向真实用户发消息，无需先打开它们的会话。
UPDATE bots SET tool_policy_json=json_set(tool_policy_json,'$.allowed_tools',json_insert(json_extract(tool_policy_json,'$.allowed_tools'),'$[#]','message_user')) WHERE desktop_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM json_each(tool_policy_json,'$.allowed_tools') WHERE value='message_user');
