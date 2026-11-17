-- 文件快照绑定真实会话，统一单聊与群聊的归属和容量；升级时需短暂暂停收发。
ALTER TABLE tombot.group_files RENAME TO chat_files;
ALTER TABLE tombot.group_file_contents RENAME TO chat_file_contents;
-- 迁移身份不是运行身份；临时策略只授权当前结构所有者，运行租户的隔离策略保持生效。
LOCK TABLE tombot.chat_files, tombot.conversations, tombot.messages IN ACCESS EXCLUSIVE MODE;
CREATE POLICY chat_library_migration ON tombot.chat_files TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY chat_library_migration ON tombot.conversations FOR SELECT TO CURRENT_USER USING (true);
CREATE POLICY chat_library_migration ON tombot.messages TO CURRENT_USER USING (true) WITH CHECK (true);
ALTER TABLE tombot.chat_files ADD COLUMN conversation_id text;
UPDATE tombot.chat_files f SET conversation_id=c.id FROM tombot.conversations c
 WHERE c.user_id=f.user_id AND c.group_id=f.group_id AND c.kind='group';
ALTER TABLE tombot.chat_files ALTER COLUMN conversation_id SET NOT NULL;
ALTER TABLE tombot.chat_files ADD CONSTRAINT chat_files_conversation_fkey
 FOREIGN KEY(user_id,conversation_id) REFERENCES tombot.conversations(user_id,id);
ALTER TABLE tombot.chat_files DROP COLUMN group_id;
ALTER TABLE tombot.chat_files ADD COLUMN is_shared boolean NOT NULL DEFAULT false;
CREATE INDEX chat_files_page ON tombot.chat_files(user_id,conversation_id,id DESC) WHERE NOT is_deleted AND is_shared;

-- 历史附件保留原显示快照；只收录实际公开发出的文件，未发送上传仍占配额但不显示。
UPDATE tombot.messages m SET blocks=(
 SELECT jsonb_agg(CASE WHEN b->>'kind'='group_file' THEN
 (b-'group_file') || jsonb_build_object('kind','chat_file','chat_file',
 ((b->'group_file')-'group_id') || jsonb_build_object('conversation_id',m.conversation_id))
 ELSE b END ORDER BY ordinal)
 FROM jsonb_array_elements(m.blocks) WITH ORDINALITY AS entries(b,ordinal)
) WHERE blocks @> '[{"kind":"group_file"}]';
UPDATE tombot.chat_files f SET is_shared=true WHERE EXISTS(
 SELECT 1 FROM tombot.messages m WHERE m.user_id=f.user_id AND m.conversation_id=f.conversation_id
 AND m.visible_to_bot_id IS NULL AND m.blocks @> jsonb_build_array(jsonb_build_object('chat_file',jsonb_build_object('id',f.id)))
);

DROP POLICY chat_library_migration ON tombot.chat_files;
DROP POLICY chat_library_migration ON tombot.conversations;
DROP POLICY chat_library_migration ON tombot.messages;
