-- 把桌面猫咪和消息从 desktop_state 的整块 JSON 拆进原来的 Bot、消息和关联表。
-- 历史行靠默认值补齐，已有 desktop_state 的内容在删表前抄进这些列。

ALTER TABLE bots ADD COLUMN desktop_id TEXT;
ALTER TABLE bots ADD COLUMN group_name TEXT NOT NULL DEFAULT '';
ALTER TABLE bots ADD COLUMN description TEXT NOT NULL DEFAULT '';
ALTER TABLE bots ADD COLUMN avatar_preset TEXT NOT NULL DEFAULT '';
ALTER TABLE bots ADD COLUMN catalog_model_id TEXT NOT NULL DEFAULT 'gpt-4o';
ALTER TABLE bots ADD COLUMN draft TEXT NOT NULL DEFAULT '';
ALTER TABLE bots ADD COLUMN computer_url TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_bots_desktop_id ON bots(desktop_id) WHERE desktop_id IS NOT NULL;

ALTER TABLE messages ADD COLUMN reply_to_id TEXT REFERENCES messages(id);
ALTER TABLE messages ADD COLUMN request_id TEXT NOT NULL DEFAULT '';

CREATE TABLE message_reactions (
  message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  emoji TEXT NOT NULL,
  count INTEGER NOT NULL CHECK (count BETWEEN 1 AND 999),
  PRIMARY KEY (message_id, emoji)
);

CREATE TABLE bot_artifacts (
  id TEXT PRIMARY KEY,
  bot_id TEXT NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
  title TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('image', 'video', 'file')),
  preview TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_bot_artifacts_bot ON bot_artifacts(bot_id, created_at DESC);

CREATE TABLE workspace_meta (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  active_agent_id TEXT NOT NULL,
  revision INTEGER NOT NULL,
  pristine INTEGER NOT NULL CHECK (pristine IN (0, 1)),
  language TEXT NOT NULL,
  provider TEXT NOT NULL,
  base_url TEXT NOT NULL,
  model_name TEXT NOT NULL,
  temperature REAL NOT NULL,
  top_p REAL NOT NULL,
  max_output_tokens TEXT NOT NULL,
  is_streaming INTEGER NOT NULL CHECK (is_streaming IN (0, 1)),
  api_key_nonce BLOB,
  api_key_ciphertext BLOB,
  updated_at INTEGER NOT NULL
);

-- 已经连上 Bot 的猫咪：字段写回那一行，桌面标识单独保存。
UPDATE bots
SET desktop_id = link.agent_id,
    name = COALESCE(json_extract(agent.value, '$.name'), bots.name),
    role_title = CASE
      WHEN COALESCE(json_extract(agent.value, '$.description'), '') = '' THEN '助手'
      ELSE substr(json_extract(agent.value, '$.description'), 1, 120)
    END,
    group_name = COALESCE(json_extract(agent.value, '$.group'), ''),
    description = COALESCE(json_extract(agent.value, '$.description'), ''),
    avatar_preset = COALESCE(json_extract(agent.value, '$.avatar'), ''),
    catalog_model_id = COALESCE(json_extract(agent.value, '$.modelId'), 'gpt-4o'),
    draft = COALESCE(json_extract(agent.value, '$.draft'), ''),
    computer_url = COALESCE(json_extract(agent.value, '$.computerUrl'), '')
FROM desktop_agent_link AS link
JOIN json_each((SELECT workspace_json FROM desktop_state WHERE id = 1), '$.agents') AS agent
  ON json_extract(agent.value, '$.id') = link.agent_id
WHERE bots.id = link.bot_id;

-- 还没有 Bot 的猫咪：用桌面标识作为 Bot 标识，避免再留一张对照表。
INSERT INTO bots (
  id, name, role_title, soul, workspace_dir, accent_hue, created_at, updated_at,
  desktop_id, group_name, description, avatar_preset, catalog_model_id, draft, computer_url
)
SELECT
  json_extract(agent.value, '$.id'),
  json_extract(agent.value, '$.name'),
  CASE
    WHEN COALESCE(json_extract(agent.value, '$.description'), '') = '' THEN '助手'
    ELSE substr(json_extract(agent.value, '$.description'), 1, 120)
  END,
  'You are the user''s cat assistant running on Tom. Think, use tools when they help, and stay inside your workspace. Reply in the user''s language.',
  'bots/' || json_extract(agent.value, '$.id'),
  (agent.key * 40) % 360,
  COALESCE((SELECT updated_at FROM desktop_state WHERE id = 1), 0),
  COALESCE((SELECT updated_at FROM desktop_state WHERE id = 1), 0),
  json_extract(agent.value, '$.id'),
  COALESCE(json_extract(agent.value, '$.group'), ''),
  COALESCE(json_extract(agent.value, '$.description'), ''),
  COALESCE(json_extract(agent.value, '$.avatar'), ''),
  COALESCE(json_extract(agent.value, '$.modelId'), 'gpt-4o'),
  COALESCE(json_extract(agent.value, '$.draft'), ''),
  COALESCE(json_extract(agent.value, '$.computerUrl'), '')
FROM json_each((SELECT workspace_json FROM desktop_state WHERE id = 1), '$.agents') AS agent
WHERE NOT EXISTS (
  SELECT 1 FROM bots WHERE bots.desktop_id = json_extract(agent.value, '$.id') OR bots.id = json_extract(agent.value, '$.id')
);

INSERT INTO conversations (id, kind, source, bot_id, title, created_at, updated_at)
SELECT 'conv-' || bots.id, 'direct', 'user', bots.id, bots.name, bots.created_at, bots.updated_at
FROM bots
WHERE bots.desktop_id IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM conversations
    WHERE conversations.bot_id = bots.id AND conversations.kind = 'direct' AND conversations.source != 'routine'
  );

INSERT INTO messages (
  id, conversation_id, seq, sender_kind, blocks_json, status, created_at, request_id
)
SELECT
  json_extract(message.value, '$.id'),
  COALESCE(
    (SELECT conversation_id FROM desktop_agent_link WHERE agent_id = json_extract(agent.value, '$.id')),
    (SELECT conversations.id FROM conversations JOIN bots ON bots.id = conversations.bot_id
      WHERE bots.desktop_id = json_extract(agent.value, '$.id') AND conversations.kind = 'direct' LIMIT 1)
  ),
  COALESCE((
    SELECT MAX(existing.seq) FROM messages AS existing
    WHERE existing.conversation_id = COALESCE(
      (SELECT conversation_id FROM desktop_agent_link WHERE agent_id = json_extract(agent.value, '$.id')),
      (SELECT conversations.id FROM conversations JOIN bots ON bots.id = conversations.bot_id
        WHERE bots.desktop_id = json_extract(agent.value, '$.id') AND conversations.kind = 'direct' LIMIT 1)
    )
  ), 0) + message.key + 1,
  CASE json_extract(message.value, '$.role') WHEN 'assistant' THEN 'bot' ELSE 'user' END,
  json_array(json_object('kind', 'text', 'text', json_extract(message.value, '$.content'))),
  COALESCE(json_extract(message.value, '$.status'), 'sent'),
  COALESCE(
    CAST(strftime('%s', json_extract(message.value, '$.createdAt')) AS INTEGER) * 1000,
    (SELECT updated_at FROM desktop_state WHERE id = 1)
  ),
  COALESCE(json_extract(message.value, '$.requestId'), '')
FROM json_each((SELECT workspace_json FROM desktop_state WHERE id = 1), '$.agents') AS agent
JOIN json_each(agent.value, '$.messages') AS message
WHERE json_extract(message.value, '$.id') IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM messages WHERE messages.id = json_extract(message.value, '$.id'))
  AND NOT EXISTS (
    SELECT 1 FROM messages AS existing
    WHERE existing.conversation_id = COALESCE(
      (SELECT conversation_id FROM desktop_agent_link WHERE agent_id = json_extract(agent.value, '$.id')),
      (SELECT conversations.id FROM conversations JOIN bots ON bots.id = conversations.bot_id
        WHERE bots.desktop_id = json_extract(agent.value, '$.id') AND conversations.kind = 'direct' LIMIT 1)
    )
      AND existing.sender_kind = CASE json_extract(message.value, '$.role') WHEN 'assistant' THEN 'bot' ELSE 'user' END
      AND instr(existing.blocks_json, json_extract(message.value, '$.content')) > 0
  );

UPDATE messages
SET reply_to_id = json_extract(message.value, '$.replyTo.messageId')
FROM json_each((SELECT workspace_json FROM desktop_state WHERE id = 1), '$.agents') AS agent
JOIN json_each(agent.value, '$.messages') AS message
WHERE messages.id = json_extract(message.value, '$.id')
  AND json_extract(message.value, '$.replyTo.messageId') IS NOT NULL
  AND EXISTS (
    SELECT 1 FROM messages AS target WHERE target.id = json_extract(message.value, '$.replyTo.messageId')
  );

INSERT INTO message_reactions (message_id, emoji, count)
SELECT
  json_extract(message.value, '$.id'),
  json_extract(reaction.value, '$.emoji'),
  json_extract(reaction.value, '$.count')
FROM json_each((SELECT workspace_json FROM desktop_state WHERE id = 1), '$.agents') AS agent
JOIN json_each(agent.value, '$.messages') AS message
JOIN json_each(message.value, '$.reactions') AS reaction
WHERE EXISTS (SELECT 1 FROM messages WHERE messages.id = json_extract(message.value, '$.id'))
  AND NOT EXISTS (
    SELECT 1 FROM message_reactions
    WHERE message_id = json_extract(message.value, '$.id') AND emoji = json_extract(reaction.value, '$.emoji')
  );

INSERT INTO bot_artifacts (id, bot_id, title, kind, preview, created_at)
SELECT
  json_extract(artifact.value, '$.id'),
  COALESCE(
    (SELECT bot_id FROM desktop_agent_link WHERE agent_id = json_extract(agent.value, '$.id')),
    (SELECT id FROM bots WHERE desktop_id = json_extract(agent.value, '$.id'))
  ),
  json_extract(artifact.value, '$.title'),
  json_extract(artifact.value, '$.kind'),
  COALESCE(json_extract(artifact.value, '$.preview'), ''),
  COALESCE(
    CAST(strftime('%s', json_extract(artifact.value, '$.createdAt')) AS INTEGER) * 1000,
    (SELECT updated_at FROM desktop_state WHERE id = 1)
  )
FROM json_each((SELECT workspace_json FROM desktop_state WHERE id = 1), '$.agents') AS agent
JOIN json_each(agent.value, '$.artifacts') AS artifact
WHERE COALESCE(
  (SELECT bot_id FROM desktop_agent_link WHERE agent_id = json_extract(agent.value, '$.id')),
  (SELECT id FROM bots WHERE desktop_id = json_extract(agent.value, '$.id'))
) IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM bot_artifacts WHERE id = json_extract(artifact.value, '$.id'));

INSERT INTO workspace_meta (
  id, active_agent_id, revision, pristine, language, provider, base_url, model_name,
  temperature, top_p, max_output_tokens, is_streaming, api_key_nonce, api_key_ciphertext, updated_at
)
SELECT
  1,
  COALESCE(json_extract(workspace_json, '$.activeAgentId'), 'assistant'),
  revision,
  pristine,
  COALESCE(json_extract(settings_json, '$.language'), 'system'),
  COALESCE(json_extract(settings_json, '$.provider'), 'openai'),
  COALESCE(json_extract(settings_json, '$.baseUrl'), ''),
  COALESCE(json_extract(settings_json, '$.modelName'), ''),
  COALESCE(json_extract(settings_json, '$.temperature'), 0.7),
  COALESCE(json_extract(settings_json, '$.topP'), 1),
  COALESCE(json_extract(settings_json, '$.maxOutputTokens'), '4096'),
  COALESCE(json_extract(settings_json, '$.isStreaming'), 1),
  api_key_nonce,
  api_key_ciphertext,
  updated_at
FROM desktop_state
WHERE id = 1;

DROP TABLE desktop_state;
DROP TABLE desktop_agent_link;
