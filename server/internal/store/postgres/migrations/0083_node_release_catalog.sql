-- 把历史分片序列映射为发布目录标识，保留发布 ID、任务外键和审计。目录授权由安装环境独立提供。
ALTER TABLE tombot.node_releases ADD COLUMN artifact text;
UPDATE tombot.node_releases r SET artifact = (
    SELECT encode(sha256(convert_to(string_agg(part, E'\n' ORDER BY sequence), 'UTF8')), 'hex')
    FROM jsonb_array_elements_text((convert_from(decode(r.envelope->>'payload', 'base64'), 'UTF8')::jsonb)->'chunks')
         WITH ORDINALITY AS parts(part, sequence)
);
ALTER TABLE tombot.node_releases ALTER COLUMN artifact SET NOT NULL,
    ADD CHECK (artifact ~ '^[0-9a-f]{64}$'), DROP COLUMN envelope;
CREATE INDEX node_releases_artifact_lookup ON tombot.node_releases (artifact, created_at DESC, id);
-- 旧更新器无法消费目录协议，新更新器轮询后重新登记可用架构。
DELETE FROM tombot.node_update_agents;
