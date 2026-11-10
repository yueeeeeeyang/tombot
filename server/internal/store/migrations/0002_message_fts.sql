-- 消息检索为派生索引；触发器保证与消息写入同一事务提交。
CREATE VIRTUAL TABLE messages_fts USING fts5(message_id UNINDEXED, conversation_id UNINDEXED, content, tokenize='trigram');
INSERT INTO messages_fts SELECT id, conversation_id, blocks_json FROM messages;
CREATE TRIGGER messages_fts_insert AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts VALUES (NEW.id, NEW.conversation_id, NEW.blocks_json);
END;
CREATE TRIGGER messages_fts_update AFTER UPDATE OF blocks_json ON messages BEGIN
  DELETE FROM messages_fts WHERE message_id=OLD.id;
  INSERT INTO messages_fts VALUES (NEW.id, NEW.conversation_id, NEW.blocks_json);
END;
CREATE TRIGGER messages_fts_delete AFTER DELETE ON messages BEGIN
  DELETE FROM messages_fts WHERE message_id=OLD.id;
END;
