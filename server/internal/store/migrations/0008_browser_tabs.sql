-- 标签只记录已确认的浏览状态；中断的变更不在启动时自动重放。
CREATE TABLE browser_tabs (
  id TEXT PRIMARY KEY,
  bot_id TEXT REFERENCES bots(id) ON DELETE RESTRICT,
  url TEXT NOT NULL CHECK(length(url) BETWEEN 1 AND 8192),
  title TEXT NOT NULL DEFAULT '',
  controller TEXT NOT NULL CHECK(controller IN('user','bot')),
  revision INTEGER NOT NULL DEFAULT 1 CHECK(revision > 0),
  page_revision INTEGER NOT NULL DEFAULT 1 CHECK(page_revision > 0),
  status TEXT NOT NULL CHECK(status IN('opening','active','acting','closing','failed')),
  error_code TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  CHECK(controller='user' OR bot_id IS NOT NULL)
);
CREATE INDEX idx_browser_tabs_bot ON browser_tabs(bot_id,id);
