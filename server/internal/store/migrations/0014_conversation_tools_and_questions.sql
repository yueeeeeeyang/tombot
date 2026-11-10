-- 检索只索引可公开的发言正文；工具参数、结果和内部状态不能经会话检索跨 Bot 传播。
CREATE VIEW message_transcripts AS
SELECT m.id AS message_id,m.conversation_id,
  COALESCE((SELECT group_concat(text,char(10)) FROM (
    SELECT json_extract(value,'$.text') AS text FROM json_each(m.blocks_json)
    WHERE json_extract(value,'$.kind') IN ('text','note') ORDER BY CAST(key AS INTEGER)
  )), '') AS content
FROM messages m;
DROP TRIGGER messages_fts_insert;
DROP TRIGGER messages_fts_update;
DROP TRIGGER messages_fts_delete;
DELETE FROM messages_fts;
INSERT INTO messages_fts SELECT message_id,conversation_id,content FROM message_transcripts;
CREATE TRIGGER messages_fts_insert AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts SELECT message_id,conversation_id,content FROM message_transcripts WHERE message_id=NEW.id;
END;
CREATE TRIGGER messages_fts_update AFTER UPDATE OF blocks_json ON messages BEGIN
  DELETE FROM messages_fts WHERE message_id=OLD.id;
  INSERT INTO messages_fts SELECT message_id,conversation_id,content FROM message_transcripts WHERE message_id=NEW.id;
END;
CREATE TRIGGER messages_fts_delete AFTER DELETE ON messages BEGIN
  DELETE FROM messages_fts WHERE message_id=OLD.id;
END;

-- 回答与审批决策原子保存，不复用审查意见字段，也不把正文写入审计日志。
ALTER TABLE approvals ADD COLUMN answer_json TEXT
  CHECK(answer_json IS NULL OR (action_class='user.question' AND json_valid(answer_json)));
CREATE INDEX idx_questions_pending ON approvals(id,run_id,expires_at)
  WHERE status='pending' AND action_class='user.question';
