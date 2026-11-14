-- 多节点可以使用同一镜像摘要，各自保留不可变的镜像登记身份。
ALTER TABLE tombot.computer_images DROP CONSTRAINT computer_images_reference_key;
CREATE INDEX computer_images_reference_lookup ON tombot.computer_images(reference);

-- 首次接入后固定地址与证书，节点重启不能改绑另一台宿主。
CREATE TABLE tombot.node_connections (
    node_id text PRIMARY KEY REFERENCES tombot.computer_nodes(id),
    url text NOT NULL UNIQUE CHECK (length(url) BETWEEN 9 AND 512 AND url LIKE 'https://%'),
    certificate_fingerprint text NOT NULL UNIQUE CHECK (certificate_fingerprint ~ '^[0-9a-f]{64}$'),
    image_id text NOT NULL REFERENCES tombot.computer_images(id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
GRANT SELECT, INSERT ON tombot.node_connections TO tombot_control;

-- 接入凭据仅保存摘要；消费与目录、连接身份在同一事务提交。
CREATE TABLE tombot.node_enrollment_tokens (
    id text PRIMARY KEY,
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash)=32),
    created_by text NOT NULL REFERENCES tombot.administrators(id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    consumed_node_id text REFERENCES tombot.computer_nodes(id),
    CHECK (expires_at > created_at)
);
CREATE INDEX node_enrollment_tokens_expiry ON tombot.node_enrollment_tokens(expires_at);
GRANT SELECT, INSERT, UPDATE, DELETE ON tombot.node_enrollment_tokens TO tombot_control;
