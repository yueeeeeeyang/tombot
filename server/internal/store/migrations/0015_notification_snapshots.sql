-- 打开客户端时只读取最新完成回合及尚可处理的审批，历史增长时仍按索引顺序取首行。
CREATE INDEX idx_notification_completed ON turns(finished_at DESC, id DESC, conversation_id)
  WHERE status='succeeded' AND source IN ('user','routine') AND finished_at IS NOT NULL;
CREATE INDEX idx_notification_pending ON approvals(status, id DESC, expires_at) WHERE status='pending';
