-- 温度、Top P、输出长度和流式开关不再由界面配置。调用时使用服务端默认值，这些列没有保留的必要。
ALTER TABLE workspace_meta DROP COLUMN temperature;
ALTER TABLE workspace_meta DROP COLUMN top_p;
ALTER TABLE workspace_meta DROP COLUMN max_output_tokens;
ALTER TABLE workspace_meta DROP COLUMN is_streaming;
