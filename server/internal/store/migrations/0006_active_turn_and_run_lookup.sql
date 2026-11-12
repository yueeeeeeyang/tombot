-- 活跃回合唯一约束防止并发请求创建两次执行；运行查询服务于调度与恢复。
CREATE UNIQUE INDEX idx_turns_active_conversation ON turns(conversation_id)
WHERE status IN ('queued','running','waiting_approval');
CREATE INDEX idx_runs_conversation_status ON runs(conversation_id,status);
CREATE INDEX idx_runs_bot_status ON runs(bot_id,status);
CREATE INDEX idx_approvals_status_id ON approvals(status,id DESC);
