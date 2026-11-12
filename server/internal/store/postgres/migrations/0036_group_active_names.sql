-- 群组与机器人采用相同的用户内活动名称规则；归档释放名称，恢复冲突必须明确失败。
-- 现有 PostgreSQL 目录若已存在重名，本事务会失败，不自动改名或丢弃资源。
CREATE UNIQUE INDEX groups_active_name ON tombot.groups(user_id,name) WHERE NOT is_archived;
