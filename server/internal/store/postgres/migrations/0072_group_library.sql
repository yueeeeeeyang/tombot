-- 群附件采用独立快照，离线电脑不影响资料读取；正文与目录共享租户边界。
CREATE TABLE tombot.group_files (
 user_id text NOT NULL,
 id text NOT NULL CHECK(id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
 group_id text NOT NULL,
 name text NOT NULL CHECK(octet_length(name) BETWEEN 1 AND 240),
 media_type text NOT NULL,
 size bigint NOT NULL CHECK(size BETWEEN 0 AND 16777216),
 sha256 text NOT NULL CHECK(sha256 ~ '^[0-9a-f]{64}$'),
 creation_digest bytea NOT NULL CHECK(octet_length(creation_digest)=32),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 is_deleted boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(user_id,id),
 UNIQUE(user_id,group_id,id),
 FOREIGN KEY(user_id,group_id) REFERENCES tombot.groups(user_id,id)
);
CREATE INDEX group_files_page ON tombot.group_files(user_id,group_id,id DESC) WHERE NOT is_deleted;
CREATE TABLE tombot.group_file_contents (
 user_id text NOT NULL,
 file_id text NOT NULL,
 content bytea NOT NULL CHECK(octet_length(content)<=16777216),
 PRIMARY KEY(user_id,file_id),
 FOREIGN KEY(user_id,file_id) REFERENCES tombot.group_files(user_id,id)
);
DO $policies$
DECLARE table_name text;
BEGIN
 FOREACH table_name IN ARRAY ARRAY['group_files','group_file_contents'] LOOP
  EXECUTE format('ALTER TABLE tombot.%I ENABLE ROW LEVEL SECURITY',table_name);
  EXECUTE format('ALTER TABLE tombot.%I FORCE ROW LEVEL SECURITY',table_name);
  EXECUTE format('CREATE POLICY tenant_scope ON tombot.%I TO tombot_tenant USING (user_id=nullif(current_setting(''tombot.user_id'',true),'''')) WITH CHECK (user_id=nullif(current_setting(''tombot.user_id'',true),''''))',table_name);
 END LOOP;
END
$policies$;
GRANT SELECT,INSERT,UPDATE ON tombot.group_files TO tombot_tenant;
GRANT SELECT,INSERT,DELETE ON tombot.group_file_contents TO tombot_tenant;
