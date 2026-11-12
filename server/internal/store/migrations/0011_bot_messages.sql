-- 同一工具调用只创建一次子执行；父链索引用于取消、费用继承和循环检测。
ALTER TABLE runs ADD COLUMN parent_tool_call_id TEXT;
ALTER TABLE runs ADD COLUMN reply_required INTEGER NOT NULL DEFAULT 0 CHECK(reply_required IN(0,1));
CREATE INDEX idx_runs_parent ON runs(parent_run_id,id);
CREATE UNIQUE INDEX idx_runs_parent_tool_call ON runs(parent_tool_call_id) WHERE parent_tool_call_id IS NOT NULL;
