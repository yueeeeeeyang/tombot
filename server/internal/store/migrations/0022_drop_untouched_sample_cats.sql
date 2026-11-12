-- 去掉第一次打开工作空间时写入的八只样例猫咪。
-- 只删除仍与样例逐字段一致、且没有消息、资料、草稿、电脑地址或其他使用痕迹的行。
-- 用户改过的猫咪、有对话的猫咪，以及用户自己招聘的猫咪都保留。

DELETE FROM conversations
WHERE bot_id IN (
    SELECT bots.id FROM bots
    WHERE bots.desktop_id IN ('assistant', 'developer', 'producer', 'artist', 'audio', 'coding', 'news', 'organizer')
      AND bots.catalog_model_id = 'gpt-4o'
      AND bots.draft = ''
      AND bots.computer_url = ''
      AND (
        (bots.desktop_id = 'assistant' AND bots.name = '小黑' AND bots.description = '你的日常搭档，陪你把想法变成现实。' AND bots.group_name = '我的助手' AND bots.avatar_preset = 'bombay')
        OR (bots.desktop_id = 'developer' AND bots.name = '游戏程序' AND bots.description = '代码、逻辑和实现，交给我一起想。' AND bots.group_name = '游戏开发' AND bots.avatar_preset = 'orange-tabby')
        OR (bots.desktop_id = 'producer' AND bots.name = '游戏负责人 + 策划' AND bots.description = '从一个好点子，到一份清晰的计划。' AND bots.group_name = '游戏开发' AND bots.avatar_preset = 'british-shorthair')
        OR (bots.desktop_id = 'artist' AND bots.name = '游戏美术' AND bots.description = '一起寻找属于这个世界的视觉语言。' AND bots.group_name = '游戏开发' AND bots.avatar_preset = 'calico')
        OR (bots.desktop_id = 'audio' AND bots.name = '游戏音效' AND bots.description = '让每一个场景，都有自己的声音。' AND bots.group_name = '游戏开发' AND bots.avatar_preset = 'siamese')
        OR (bots.desktop_id = 'coding' AND bots.name = '编程搭档' AND bots.description = '聊聊代码，也聊聊下一步怎么做。' AND bots.group_name = '未分组' AND bots.avatar_preset = 'american-shorthair')
        OR (bots.desktop_id = 'news' AND bots.name = '资讯助手' AND bots.description = '为你整理值得关注的新鲜事。' AND bots.group_name = '未分组' AND bots.avatar_preset = 'ragdoll')
        OR (bots.desktop_id = 'organizer' AND bots.name = '整理助手' AND bots.description = '少一点杂乱，多一点井井有条。' AND bots.group_name = '未分组' AND bots.avatar_preset = 'persian')
      )
      AND NOT EXISTS (SELECT 1 FROM messages JOIN conversations AS owned ON owned.id = messages.conversation_id WHERE owned.bot_id = bots.id)
      AND NOT EXISTS (SELECT 1 FROM bot_artifacts WHERE bot_artifacts.bot_id = bots.id)
      AND NOT EXISTS (SELECT 1 FROM group_members WHERE group_members.bot_id = bots.id)
      AND NOT EXISTS (SELECT 1 FROM routines WHERE routines.bot_id = bots.id)
      AND NOT EXISTS (SELECT 1 FROM runs WHERE runs.bot_id = bots.id)
      AND NOT EXISTS (SELECT 1 FROM browser_tabs WHERE browser_tabs.bot_id = bots.id)
      AND NOT EXISTS (SELECT 1 FROM terminal_sessions WHERE terminal_sessions.bot_id = bots.id)
  )
  AND NOT EXISTS (SELECT 1 FROM messages WHERE messages.conversation_id = conversations.id)
  AND NOT EXISTS (SELECT 1 FROM turns WHERE turns.conversation_id = conversations.id)
  AND NOT EXISTS (SELECT 1 FROM runs WHERE runs.conversation_id = conversations.id);

DELETE FROM bots
WHERE desktop_id IN ('assistant', 'developer', 'producer', 'artist', 'audio', 'coding', 'news', 'organizer')
  AND catalog_model_id = 'gpt-4o'
  AND draft = ''
  AND computer_url = ''
  AND (
    (desktop_id = 'assistant' AND name = '小黑' AND description = '你的日常搭档，陪你把想法变成现实。' AND group_name = '我的助手' AND avatar_preset = 'bombay')
    OR (desktop_id = 'developer' AND name = '游戏程序' AND description = '代码、逻辑和实现，交给我一起想。' AND group_name = '游戏开发' AND avatar_preset = 'orange-tabby')
    OR (desktop_id = 'producer' AND name = '游戏负责人 + 策划' AND description = '从一个好点子，到一份清晰的计划。' AND group_name = '游戏开发' AND avatar_preset = 'british-shorthair')
    OR (desktop_id = 'artist' AND name = '游戏美术' AND description = '一起寻找属于这个世界的视觉语言。' AND group_name = '游戏开发' AND avatar_preset = 'calico')
    OR (desktop_id = 'audio' AND name = '游戏音效' AND description = '让每一个场景，都有自己的声音。' AND group_name = '游戏开发' AND avatar_preset = 'siamese')
    OR (desktop_id = 'coding' AND name = '编程搭档' AND description = '聊聊代码，也聊聊下一步怎么做。' AND group_name = '未分组' AND avatar_preset = 'american-shorthair')
    OR (desktop_id = 'news' AND name = '资讯助手' AND description = '为你整理值得关注的新鲜事。' AND group_name = '未分组' AND avatar_preset = 'ragdoll')
    OR (desktop_id = 'organizer' AND name = '整理助手' AND description = '少一点杂乱，多一点井井有条。' AND group_name = '未分组' AND avatar_preset = 'persian')
  )
  AND NOT EXISTS (SELECT 1 FROM messages JOIN conversations AS owned ON owned.id = messages.conversation_id WHERE owned.bot_id = bots.id)
  AND NOT EXISTS (SELECT 1 FROM conversations WHERE conversations.bot_id = bots.id)
  AND NOT EXISTS (SELECT 1 FROM bot_artifacts WHERE bot_artifacts.bot_id = bots.id)
  AND NOT EXISTS (SELECT 1 FROM group_members WHERE group_members.bot_id = bots.id)
  AND NOT EXISTS (SELECT 1 FROM routines WHERE routines.bot_id = bots.id)
  AND NOT EXISTS (SELECT 1 FROM runs WHERE runs.bot_id = bots.id)
  AND NOT EXISTS (SELECT 1 FROM browser_tabs WHERE browser_tabs.bot_id = bots.id)
  AND NOT EXISTS (SELECT 1 FROM terminal_sessions WHERE terminal_sessions.bot_id = bots.id);

UPDATE workspace_meta
SET active_agent_id = COALESCE(
  (SELECT desktop_id FROM bots WHERE desktop_id IS NOT NULL AND is_archived = 0 ORDER BY created_at, id LIMIT 1),
  ''
)
WHERE id = 1
  AND NOT EXISTS (
    SELECT 1 FROM bots
    WHERE is_archived = 0 AND (desktop_id = workspace_meta.active_agent_id OR id = workspace_meta.active_agent_id)
  );
