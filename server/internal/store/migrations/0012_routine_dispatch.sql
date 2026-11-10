-- 主会话保持唯一，例程新建会话不占用主会话身份。
DROP INDEX idx_conversations_direct;
DROP INDEX idx_conversations_group;
CREATE UNIQUE INDEX idx_conversations_direct ON conversations(bot_id) WHERE kind='direct' AND source!='routine';
CREATE UNIQUE INDEX idx_conversations_group ON conversations(group_id) WHERE kind='group' AND source!='routine';
CREATE INDEX idx_conversations_group_history ON conversations(group_id,created_at);

ALTER TABLE routines ADD COLUMN revision INTEGER NOT NULL DEFAULT 1 CHECK(revision>0);
ALTER TABLE routines ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_routines_due ON routines(next_run_at,id) WHERE is_enabled=1;
ALTER TABLE routine_runs ADD COLUMN conversation_id TEXT REFERENCES conversations(id) ON DELETE SET NULL;
ALTER TABLE routine_runs ADD COLUMN definition_json TEXT;
ALTER TABLE routine_runs ADD COLUMN error_code TEXT;
ALTER TABLE routine_runs ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX idx_routine_schedule_once ON routine_runs(routine_id,scheduled_at) WHERE trigger_kind!='manual';
CREATE INDEX idx_routine_pending ON routine_runs(created_at,id) WHERE status='pending';

-- 例程配置审批没有模型 Run；保留原有工具审批字段和历史记录，禁止其他无 Run 待办。
CREATE TABLE approvals_next (
  id TEXT PRIMARY KEY,
  run_id TEXT REFERENCES runs(id),
  tool_call_id TEXT,
  bot_id TEXT,
  action_class TEXT NOT NULL,
  title TEXT NOT NULL,
  detail_json TEXT NOT NULL,
  risk_level TEXT NOT NULL,
  reviewer_note TEXT,
  status TEXT NOT NULL,
  decided_at INTEGER,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  CHECK((run_id IS NULL AND tool_call_id IS NULL AND action_class='routine.create') OR
        (run_id IS NOT NULL AND tool_call_id IS NOT NULL AND bot_id IS NOT NULL))
);
INSERT INTO approvals_next SELECT * FROM approvals;
DROP TABLE approvals;
ALTER TABLE approvals_next RENAME TO approvals;
CREATE INDEX idx_approvals_pending ON approvals(status) WHERE status='pending';
CREATE TABLE routine_changes (
  approval_id TEXT PRIMARY KEY REFERENCES approvals(id) ON DELETE CASCADE,
  routine_id TEXT NOT NULL,
  expected_revision INTEGER NOT NULL CHECK(expected_revision>=0),
  definition_json TEXT NOT NULL
);
CREATE INDEX idx_routine_changes_target ON routine_changes(routine_id);
