-- 先按会话与读取游标定位包含产物的候选消息，再解析真实消息块，避免反复扫描长转录。
CREATE INDEX idx_messages_inbox_artifacts ON messages(conversation_id,seq)
  WHERE sender_kind!='user' AND instr(blocks_json,'"artifact"')>0;
CREATE INDEX idx_turns_inbox_active ON turns(conversation_id)
  WHERE status IN ('queued','running');
