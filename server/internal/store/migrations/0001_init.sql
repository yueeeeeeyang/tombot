CREATE TABLE system_state (
  key        TEXT PRIMARY KEY,
  value_json TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE capabilities (
  key        TEXT PRIMARY KEY,
  status     TEXT NOT NULL,          -- ok | degraded | failed | skipped
  version    TEXT,
  detail     TEXT,
  checked_at INTEGER NOT NULL
);

CREATE TABLE providers (
  id           TEXT PRIMARY KEY,
  kind         TEXT NOT NULL,        -- anthropic | openai_compat
  display_name TEXT NOT NULL,
  base_url     TEXT,
  secret_id    TEXT,                 -- 可空（本地无鉴权端点）
  is_enabled   INTEGER NOT NULL DEFAULT 1,
  extra_json   TEXT NOT NULL DEFAULT '{}',
  created_at   INTEGER NOT NULL
);

CREATE TABLE models (
  id                    TEXT PRIMARY KEY,
  provider_id           TEXT NOT NULL REFERENCES providers(id),
  model_key             TEXT NOT NULL,
  display_name          TEXT NOT NULL,
  context_window        INTEGER NOT NULL,
  max_output            INTEGER NOT NULL,
  input_price_micro     INTEGER NOT NULL,     -- 微美元 / 百万 token（micro-USD per 1M tokens）
  output_price_micro    INTEGER NOT NULL,
  cache_read_price_micro INTEGER,
  supports_tools        INTEGER NOT NULL DEFAULT 1,
  supports_vision       INTEGER NOT NULL DEFAULT 1,
  is_default            INTEGER NOT NULL DEFAULT 0,
  created_at            INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_models_provider_key ON models(provider_id, model_key);

CREATE TABLE bots (
  id                TEXT PRIMARY KEY,
  name              TEXT NOT NULL,
  avatar_path       TEXT,
  role_title        TEXT NOT NULL,          -- 一句话职责，界面展示
  soul              TEXT NOT NULL,          -- 角色提示词（英文）
  model_id          TEXT REFERENCES models(id),
  tool_policy_json  TEXT NOT NULL DEFAULT '{}',
  messaging_policy_json TEXT NOT NULL DEFAULT '{}',  -- {accept_dm, can_initiate, accept_group_invite}
  workspace_dir     TEXT NOT NULL,
  accent_hue        INTEGER NOT NULL,       -- 0..359，界面身份色
  is_archived       INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_bots_name ON bots(name) WHERE is_archived = 0;

CREATE TABLE groups (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL,
  purpose       TEXT NOT NULL DEFAULT '',   -- 群组用途说明，注入每个成员的群上下文
  coordinator_bot_id TEXT REFERENCES bots(id) ON DELETE SET NULL,
  routing       TEXT NOT NULL DEFAULT 'mention',  -- mention | coordinator | all
  max_speakers  INTEGER NOT NULL DEFAULT 3,
  max_cost_micro INTEGER,                   -- 单回合成本上限
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE group_members (
  group_id  TEXT NOT NULL REFERENCES groups(id),
  bot_id    TEXT NOT NULL REFERENCES bots(id),
  role_note TEXT NOT NULL DEFAULT '',       -- 在本群的分工备注
  joined_at INTEGER NOT NULL,
  PRIMARY KEY (group_id, bot_id)
);

CREATE TABLE conversations (
  id           TEXT PRIMARY KEY,
  kind         TEXT NOT NULL,        -- direct | group | bot_dm
  source       TEXT NOT NULL DEFAULT 'user',   -- user | bot | routine | delegation（区分委派与闲聊产生的 bot_dm）
  bot_id       TEXT REFERENCES bots(id),       -- direct / bot_dm 的对方（bot_dm 存“另一方”）
  group_id     TEXT REFERENCES groups(id),     -- group 会话
  initiator_bot_id TEXT,                       -- bot_dm 的发起方
  title        TEXT NOT NULL,
  summary      TEXT NOT NULL DEFAULT '',
  summary_upto_seq INTEGER NOT NULL DEFAULT 0,
  last_read_seq INTEGER NOT NULL DEFAULT 0,    -- 左栏未读计算的游标
  is_archived  INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);
CREATE INDEX idx_conversations_updated ON conversations(updated_at DESC);
CREATE INDEX idx_conversations_bot ON conversations(bot_id, updated_at DESC);
CREATE UNIQUE INDEX idx_conversations_group ON conversations(group_id) WHERE kind = 'group';
-- direct 会话与 Bot 一一对应；闲聊型 bot_dm 按有序对唯一（两端必须非空，服务层校验）
CREATE UNIQUE INDEX idx_conversations_direct ON conversations(bot_id) WHERE kind = 'direct';
CREATE UNIQUE INDEX idx_conversations_dm ON conversations(
  MIN(initiator_bot_id, bot_id), MAX(initiator_bot_id, bot_id)
) WHERE kind = 'bot_dm' AND source != 'delegation';
-- 委派会话（source='delegation'）不参与上面的唯一约束：每次委派独立成会话，可多次并存

CREATE TABLE turns (
  id              TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  seq             INTEGER NOT NULL,
  source          TEXT NOT NULL,      -- user | routine | bot_message
  status          TEXT NOT NULL,      -- queued|running|waiting_approval|succeeded|failed|canceled|interrupted
  plan_json       TEXT,               -- 群组分工计划
  error_code      TEXT,
  cost_micro      INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL,
  finished_at     INTEGER
);
CREATE UNIQUE INDEX idx_turns_seq ON turns(conversation_id, seq);
CREATE INDEX idx_turns_status ON turns(status) WHERE status IN ('queued','running','waiting_approval');

CREATE TABLE messages (
  id              TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  turn_id         TEXT,
  run_id          TEXT,
  seq             INTEGER NOT NULL,
  sender_kind     TEXT NOT NULL,      -- user | bot | system
  sender_bot_id   TEXT,
  blocks_json     TEXT NOT NULL,
  status          TEXT NOT NULL DEFAULT 'complete',  -- streaming | complete | interrupted | canceled
  token_in        INTEGER NOT NULL DEFAULT 0,
  token_out       INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_messages_seq ON messages(conversation_id, seq);

CREATE TABLE runs (
  id              TEXT PRIMARY KEY,
  turn_id         TEXT NOT NULL REFERENCES turns(id),
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  bot_id          TEXT NOT NULL REFERENCES bots(id),
  parent_run_id   TEXT,               -- 委派/私信时指向调用方 Run
  source          TEXT NOT NULL,      -- chat | group | routine | delegation | bot_message
  status          TEXT NOT NULL,      -- queued|running|waiting_approval|succeeded|failed|canceled|interrupted
  model_id        TEXT NOT NULL,
  prompt          TEXT NOT NULL,
  plan_role       TEXT,               -- coordinator | member | solo
  error_code      TEXT,
  error_message   TEXT,
  cost_micro      INTEGER NOT NULL DEFAULT 0,
  budget_micro    INTEGER,
  hop_depth       INTEGER NOT NULL DEFAULT 0,
  started_at      INTEGER,
  finished_at     INTEGER,
  created_at      INTEGER NOT NULL
);
CREATE INDEX idx_runs_turn ON runs(turn_id, created_at);
CREATE INDEX idx_runs_bot ON runs(bot_id, created_at DESC);
CREATE INDEX idx_runs_status ON runs(status) WHERE status IN ('queued','running','waiting_approval');

CREATE TABLE tool_calls (
  id            TEXT PRIMARY KEY,
  run_id        TEXT NOT NULL REFERENCES runs(id),
  step          INTEGER NOT NULL,
  tool_name     TEXT NOT NULL,
  input_json    TEXT NOT NULL,       -- 已脱敏
  output_json   TEXT,
  status        TEXT NOT NULL,       -- pending|awaiting_approval|running|succeeded|failed|denied|canceled
  error_code    TEXT,
  approval_id   TEXT,
  duration_ms   INTEGER,
  created_at    INTEGER NOT NULL,
  finished_at   INTEGER
);
CREATE INDEX idx_tool_calls_run ON tool_calls(run_id, step);

CREATE TABLE approvals (
  id           TEXT PRIMARY KEY,
  run_id       TEXT NOT NULL REFERENCES runs(id),
  tool_call_id TEXT NOT NULL,
  bot_id       TEXT NOT NULL,
  action_class TEXT NOT NULL,
  title        TEXT NOT NULL,        -- 中文
  detail_json  TEXT NOT NULL,
  risk_level   TEXT NOT NULL,
  reviewer_note TEXT,
  status       TEXT NOT NULL,        -- pending|approved|denied|expired
  decided_at   INTEGER,
  expires_at   INTEGER NOT NULL,
  created_at   INTEGER NOT NULL
);
CREATE INDEX idx_approvals_pending ON approvals(status) WHERE status = 'pending';

CREATE TABLE policy_rules (
  id           TEXT PRIMARY KEY,
  bot_id       TEXT NOT NULL,        -- '*' 表示所有 Bot
  action_class TEXT NOT NULL,
  pattern      TEXT,
  decision     TEXT NOT NULL,        -- allow | deny
  note         TEXT,
  created_at   INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_policy_unique ON policy_rules(bot_id, action_class, COALESCE(pattern,''));

CREATE TABLE routines (
  id               TEXT PRIMARY KEY,
  name             TEXT NOT NULL,
  bot_id           TEXT REFERENCES bots(id),        -- direct 目标
  group_id         TEXT REFERENCES groups(id),      -- 群组目标
  cron             TEXT NOT NULL,
  timezone         TEXT NOT NULL DEFAULT 'Asia/Shanghai',
  prompt           TEXT NOT NULL,
  overlap_policy   TEXT NOT NULL DEFAULT 'skip',
  catch_up         TEXT NOT NULL DEFAULT 'once',
  target_mode      TEXT NOT NULL DEFAULT 'new',     -- new | pinned
  pinned_conversation_id TEXT,
  is_enabled       INTEGER NOT NULL DEFAULT 1,
  next_run_at      INTEGER,
  last_run_at      INTEGER,
  created_at       INTEGER NOT NULL
);
-- 约束：bot_id 与 group_id 恰有一个非空（应用层校验 + CHECK）
CREATE TRIGGER trg_routines_target_insert BEFORE INSERT ON routines
WHEN (NEW.bot_id IS NULL) = (NEW.group_id IS NULL)
BEGIN SELECT RAISE(ABORT, 'routine_target_invalid'); END;
CREATE TRIGGER trg_routines_target_update BEFORE UPDATE ON routines
WHEN (NEW.bot_id IS NULL) = (NEW.group_id IS NULL)
BEGIN SELECT RAISE(ABORT, 'routine_target_invalid'); END;

CREATE TABLE routine_runs (
  id           TEXT PRIMARY KEY,
  routine_id   TEXT NOT NULL REFERENCES routines(id),
  turn_id      TEXT,
  trigger_kind TEXT NOT NULL,        -- schedule | manual | catch_up
  status       TEXT NOT NULL,        -- triggered | skipped_overlap | failed
  scheduled_at INTEGER NOT NULL,
  created_at   INTEGER NOT NULL
);
CREATE INDEX idx_routine_runs ON routine_runs(routine_id, scheduled_at DESC);

CREATE TABLE secrets (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  ciphertext BLOB NOT NULL,
  nonce      BLOB NOT NULL,
  hint       TEXT,
  created_at INTEGER NOT NULL,
  rotated_at INTEGER
);
CREATE UNIQUE INDEX idx_secrets_name ON secrets(name);

-- 已授权客户端：只存令牌哈希，明文仅登录时返回一次
CREATE TABLE devices (
  id             TEXT PRIMARY KEY,
  name           TEXT NOT NULL,        -- 客户端上报的显示名，如 "MacBook Pro"
  platform       TEXT NOT NULL,        -- macos | windows | linux | ios | android
  token_hash     TEXT NOT NULL,        -- sha256(device_token)
  token_hint     TEXT NOT NULL,        -- 前 4 + 后 4，用于列表展示
  push_token     TEXT,                 -- 移动推送令牌，可空
  last_seen_at   INTEGER,
  created_at     INTEGER NOT NULL,
  revoked_at     INTEGER
);
CREATE UNIQUE INDEX idx_devices_token ON devices(token_hash);
CREATE INDEX idx_devices_active ON devices(created_at DESC) WHERE revoked_at IS NULL;

CREATE TABLE usage_events (
  id                TEXT PRIMARY KEY,
  run_id            TEXT,
  turn_id           TEXT,
  bot_id            TEXT,
  model_id          TEXT NOT NULL,
  input_tokens      INTEGER NOT NULL DEFAULT 0,
  output_tokens     INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens INTEGER NOT NULL DEFAULT 0,
  cost_micro        INTEGER NOT NULL DEFAULT 0,
  is_estimated      INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL
);
CREATE INDEX idx_usage_time ON usage_events(created_at);
CREATE INDEX idx_usage_bot ON usage_events(bot_id, created_at);

CREATE TABLE memory_index (
  bot_id       TEXT NOT NULL,
  path         TEXT NOT NULL,
  title        TEXT NOT NULL,
  summary      TEXT NOT NULL,
  tags         TEXT NOT NULL DEFAULT '',
  importance   INTEGER NOT NULL DEFAULT 3,
  size_bytes   INTEGER NOT NULL,
  content_hash TEXT NOT NULL,
  updated_at   INTEGER NOT NULL,
  last_used_at INTEGER,
  PRIMARY KEY (bot_id, path)
);

-- 普通 FTS 表（非 external content），由索引维护器在同一事务内同步写入
CREATE VIRTUAL TABLE memory_fts_ascii USING fts5(
  bot_id UNINDEXED, path UNINDEXED, title, summary, tags, content,
  tokenize='porter unicode61'
);
CREATE VIRTUAL TABLE memory_fts_cjk USING fts5(
  bot_id UNINDEXED, path UNINDEXED, title, summary, tags, content,
  tokenize='trigram'
);

CREATE TABLE audit_log (
  id          TEXT PRIMARY KEY,
  actor       TEXT NOT NULL,         -- user | bot:<id> | system | routine:<id>
  action      TEXT NOT NULL,
  target      TEXT,
  detail_json TEXT NOT NULL DEFAULT '{}',
  ip          TEXT,
  created_at  INTEGER NOT NULL
);
CREATE INDEX idx_audit_time ON audit_log(created_at DESC);

CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value_json TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  checksum   TEXT NOT NULL,
  applied_at INTEGER NOT NULL
);
