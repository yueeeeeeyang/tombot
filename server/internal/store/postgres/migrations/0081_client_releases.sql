-- 只在安装包完整后登记发布记录。唯一索引同时用于按平台查找最高版本。
CREATE TABLE tombot.client_releases (
    id text PRIMARY KEY,
    platform text NOT NULL,
    version_number bigint NOT NULL CHECK (version_number >= 0),
    manifest jsonb NOT NULL CHECK (jsonb_typeof(manifest) = 'object'),
    UNIQUE (platform, version_number)
);
GRANT SELECT, INSERT ON tombot.client_releases TO tombot_control;
