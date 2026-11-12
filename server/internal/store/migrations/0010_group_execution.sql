-- 群配置版本保护编辑竞争；汇总开关与回合预算不改变既有单聊行为。
ALTER TABLE groups ADD COLUMN revision INTEGER NOT NULL DEFAULT 1 CHECK(revision>0);
ALTER TABLE groups ADD COLUMN auto_summary INTEGER NOT NULL DEFAULT 1 CHECK(auto_summary IN(0,1));
ALTER TABLE turns ADD COLUMN budget_micro INTEGER CHECK(budget_micro IS NULL OR budget_micro>0);
ALTER TABLE turns ADD COLUMN group_snapshot_json TEXT;
ALTER TABLE turns ADD COLUMN history_upto_seq INTEGER CHECK(history_upto_seq IS NULL OR history_upto_seq>=0);
CREATE INDEX idx_group_members_bot ON group_members(bot_id,group_id);
CREATE INDEX idx_groups_cursor ON groups(id DESC);
-- 持久角色由运行时负责，允许读取同一回合中其他成员的终态而不提前结束回合。
CREATE INDEX idx_runs_turn_status ON runs(turn_id,status);
