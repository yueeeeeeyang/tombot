-- 用户接管独占单个 Bot；身份、租约与控制序号持久化，窗口画面和输入正文不入库。
ALTER TABLE tombot.runs ADD COLUMN accepts_console boolean NOT NULL DEFAULT true;
ALTER TABLE tombot.turns DROP CONSTRAINT turns_source_check;
ALTER TABLE tombot.turns ADD CONSTRAINT turns_source_check CHECK(source IN ('chat','group','bot_dm','proactive','routine','console'));
DROP INDEX tombot.turns_active_conversation;
CREATE UNIQUE INDEX turns_active_conversation ON tombot.turns(user_id,conversation_id)
 WHERE status IN ('queued','running','canceling') AND source NOT IN ('group','console');

CREATE TABLE tombot.desktop_console_heads (
 user_id text NOT NULL,
 bot_id text NOT NULL,
 revision bigint NOT NULL DEFAULT 0 CHECK(revision>=0),
 PRIMARY KEY(user_id,bot_id),
 FOREIGN KEY(user_id,bot_id) REFERENCES tombot.bots(user_id,id)
);

CREATE TABLE tombot.desktop_consoles (
 user_id text NOT NULL,
 id text NOT NULL CHECK(id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
 bot_id text NOT NULL,
 device_id text NOT NULL,
 client_request_id text NOT NULL CHECK(client_request_id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
 run_id text NOT NULL,
 computer_session_id text,
 control_revision bigint NOT NULL CHECK(control_revision>0),
 input_sequence bigint NOT NULL DEFAULT 0 CHECK(input_sequence>=0),
 capture_sequence bigint NOT NULL DEFAULT 0 CHECK(capture_sequence>=0),
 state text NOT NULL CHECK(state IN ('waiting','pausing','active','resuming','closed','failed')),
 error_code text NOT NULL DEFAULT '' CHECK(error_code='' OR error_code ~ '^[A-Z][A-Z0-9_]{0,63}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL DEFAULT clock_timestamp()+interval '30 seconds',
 PRIMARY KEY(user_id,id),
 UNIQUE(user_id,client_request_id),
 FOREIGN KEY(user_id,bot_id) REFERENCES tombot.bots(user_id,id),
 FOREIGN KEY(user_id,device_id) REFERENCES tombot.devices(user_id,id),
 FOREIGN KEY(user_id,run_id,bot_id) REFERENCES tombot.runs(user_id,id,bot_id),
 FOREIGN KEY(user_id,computer_session_id) REFERENCES tombot.computer_sessions(user_id,id)
);
CREATE UNIQUE INDEX desktop_consoles_active_bot ON tombot.desktop_consoles(user_id,bot_id)
 WHERE state IN ('waiting','pausing','active','resuming');
CREATE INDEX desktop_consoles_run ON tombot.desktop_consoles(user_id,run_id,created_at DESC);

DO $policies$
DECLARE relation text;
BEGIN
 FOREACH relation IN ARRAY ARRAY['desktop_console_heads','desktop_consoles'] LOOP
  EXECUTE format('ALTER TABLE tombot.%I ENABLE ROW LEVEL SECURITY',relation);
  EXECUTE format('ALTER TABLE tombot.%I FORCE ROW LEVEL SECURITY',relation);
  EXECUTE format('CREATE POLICY tenant_scope ON tombot.%I TO tombot_tenant USING(user_id=nullif(current_setting(''tombot.user_id'',true),'''')) WITH CHECK(user_id=nullif(current_setting(''tombot.user_id'',true),''''))',relation);
  EXECUTE format('GRANT SELECT,INSERT,UPDATE ON tombot.%I TO tombot_tenant',relation);
 END LOOP;
END
$policies$;
