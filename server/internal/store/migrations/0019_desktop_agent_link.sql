-- 猫咪与原来的 Bot、单聊是同一身份。执行、文件和浏览器都走这条链接，不再另建执行对象。
CREATE TABLE desktop_agent_link (
  agent_id TEXT PRIMARY KEY,
  bot_id TEXT NOT NULL UNIQUE REFERENCES bots(id),
  conversation_id TEXT NOT NULL UNIQUE REFERENCES conversations(id)
);
