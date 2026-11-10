-- 过期令牌必须可撤销；事件游标持久化后跨重启仍然单调。
ALTER TABLE devices ADD COLUMN expires_at INTEGER;
CREATE TABLE events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  conversation_id TEXT NOT NULL DEFAULT '',
  payload_json TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_events_conversation ON events(conversation_id, id);
