-- 只持久化会话身份与游标，不保存输入、输出或进程恢复指令。
CREATE TABLE terminal_sessions (
  sequence INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT NOT NULL UNIQUE,
  owner TEXT NOT NULL,
  bot_id TEXT REFERENCES bots(id) ON DELETE CASCADE,
  run_id TEXT REFERENCES runs(id) ON DELETE SET NULL,
  title TEXT NOT NULL,
  cwd TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN('starting','running','closing','exited','closed','interrupted')),
  controller TEXT NOT NULL CHECK(controller IN('user','bot')),
  revision INTEGER NOT NULL DEFAULT 1 CHECK(revision > 0),
  columns INTEGER NOT NULL CHECK(columns BETWEEN 2 AND 500),
  rows INTEGER NOT NULL CHECK(rows BETWEEN 2 AND 300),
  read_cursor INTEGER NOT NULL DEFAULT 0 CHECK(read_cursor >= 0),
  exit_code INTEGER,
  error_code TEXT,
  created_at INTEGER NOT NULL,
  finished_at INTEGER,
  CHECK(owner='user' OR (bot_id IS NOT NULL AND owner='bot:' || bot_id)),
  CHECK(controller='user' OR bot_id IS NOT NULL)
);
CREATE INDEX idx_terminal_sessions_bot ON terminal_sessions(bot_id, id DESC);
CREATE INDEX idx_terminal_sessions_run ON terminal_sessions(run_id) WHERE run_id IS NOT NULL;
CREATE INDEX idx_terminal_sessions_active ON terminal_sessions(status, id) WHERE status IN('starting','running','closing','exited');
