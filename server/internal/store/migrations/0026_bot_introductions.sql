-- 展示介绍单独缓存，不写回身份、岗位或记忆；调用用量独立累计。
CREATE TABLE bot_introductions (
  bot_id TEXT PRIMARY KEY REFERENCES bots(id) ON DELETE CASCADE,
  source_digest TEXT NOT NULL DEFAULT '',
  text TEXT NOT NULL DEFAULT '',
  input_tokens INTEGER NOT NULL DEFAULT 0 CHECK(input_tokens >= 0),
  output_tokens INTEGER NOT NULL DEFAULT 0 CHECK(output_tokens >= 0),
  cost_micro INTEGER NOT NULL DEFAULT 0 CHECK(cost_micro >= 0),
  updated_at INTEGER NOT NULL DEFAULT 0
);

-- 旧桌面介绍曾兼作岗位提示；停止让展示文字反向决定工作内容。
UPDATE bots SET role_title='助手' WHERE desktop_id IS NOT NULL;
