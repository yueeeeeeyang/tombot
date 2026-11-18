-- 发布文件按摘要存储在发布目录；数据库仅保存签名清单和每节点最后一次任务，空间随节点数有界。
CREATE TABLE tombot.node_releases (
 id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{64}$'), label text NOT NULL, platform text NOT NULL,
 envelope jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE tombot.node_update_agents (
 node_id text PRIMARY KEY REFERENCES tombot.node_connections(node_id), platform text NOT NULL,
 last_seen_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE tombot.node_update_jobs (
 node_id text PRIMARY KEY REFERENCES tombot.node_connections(node_id), id text NOT NULL UNIQUE,
 release_id text NOT NULL REFERENCES tombot.node_releases(id), state text NOT NULL
 CHECK (state IN ('queued','running','started','busy','unsupported','failed')),
 applied_image text REFERENCES tombot.computer_images(id),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
GRANT SELECT, INSERT ON tombot.node_releases TO tombot_control;
GRANT SELECT, INSERT, UPDATE ON tombot.node_update_agents,tombot.node_update_jobs TO tombot_control;
