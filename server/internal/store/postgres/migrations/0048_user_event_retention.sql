-- 事件只提示客户端重新读取资源，快照才是权威状态，因此历史事件可以清理。
-- 每用户保留最近一段序号；被清理的推进边界记录在事件头，游标落后时服务端明确要求重新同步，
-- 不能静默跳过已删除的事件。
ALTER TABLE tombot.user_event_heads
    ADD COLUMN pruned_through bigint NOT NULL DEFAULT 0 CHECK (pruned_through >= 0);
ALTER TABLE tombot.user_event_heads
    ADD CONSTRAINT user_event_heads_pruned CHECK (pruned_through <= sequence);

-- 清理由控制连接执行：它需要定位必须保留的序号并删除更早的事件。
-- 该连接不读取事件内容以外的业务表，也不会写入事件。
GRANT SELECT ON tombot.user_events TO tombot_control;
GRANT SELECT, UPDATE ON tombot.user_event_heads TO tombot_control;
GRANT DELETE ON tombot.user_events TO tombot_control;
CREATE POLICY control_event_heads ON tombot.user_event_heads TO tombot_control USING (true) WITH CHECK (true);
CREATE POLICY control_events_prune ON tombot.user_events FOR DELETE TO tombot_control USING (true);
CREATE POLICY control_events_read ON tombot.user_events FOR SELECT TO tombot_control USING (true);
