-- 观看保持任务运行，接管在操作边界暂停；同一连接交还后继续观看。
ALTER TABLE tombot.desktop_consoles DROP CONSTRAINT desktop_consoles_state_check;
ALTER TABLE tombot.desktop_consoles ADD CONSTRAINT desktop_consoles_state_check
 CHECK(state IN ('opening','viewing','waiting','pausing','active','resuming','closed','failed'));
ALTER TABLE tombot.desktop_consoles ADD COLUMN return_to_view boolean NOT NULL DEFAULT false;
DROP INDEX tombot.desktop_consoles_active_bot;
CREATE UNIQUE INDEX desktop_consoles_active_bot ON tombot.desktop_consoles(user_id,bot_id)
 WHERE state IN ('opening','viewing','waiting','pausing','active','resuming');
