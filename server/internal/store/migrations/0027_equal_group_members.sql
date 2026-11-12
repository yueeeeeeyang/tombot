-- 群成员平等：删除协调和分工配置，保留历史回合与消息。
ALTER TABLE groups DROP COLUMN coordinator_bot_id;
ALTER TABLE groups DROP COLUMN routing;
ALTER TABLE groups DROP COLUMN auto_summary;
ALTER TABLE groups DROP COLUMN max_speakers;
ALTER TABLE group_members DROP COLUMN role_note;
ALTER TABLE groups ADD COLUMN avatar_presets_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(avatar_presets_json) AND json_type(avatar_presets_json)='array');
-- 历史群无法还原已移出的初始成员，采用升级时最早加入的四位；之后固定不变。
UPDATE groups SET avatar_presets_json=COALESCE((SELECT json_group_array(preset) FROM (SELECT CASE WHEN b.avatar_preset='' THEN 'bombay' ELSE b.avatar_preset END AS preset FROM group_members m JOIN bots b ON b.id=m.bot_id WHERE m.group_id=groups.id ORDER BY m.joined_at,b.id LIMIT 4)),'[]');
ALTER TABLE messages ADD COLUMN audience_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(audience_json) AND json_type(audience_json)='array');
ALTER TABLE messages ADD COLUMN mentions_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(mentions_json) AND json_type(mentions_json)='array');
ALTER TABLE runs ADD COLUMN trigger_message_id TEXT;
-- 过去的共用摘要没有可见范围信息，停止把它提供给群成员。
UPDATE conversations SET summary='',summary_upto_seq=0 WHERE kind='group';
