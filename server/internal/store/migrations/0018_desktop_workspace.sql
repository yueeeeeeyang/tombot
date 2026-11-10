-- 桌面客户端的工作空间与模型设置。
-- 界面字段（分组、草稿、表情、资料库、电脑地址）与 Bot 执行引擎的消息块不是同一套结构，单独存放，避免改写历史表。
CREATE TABLE desktop_state (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  workspace_json TEXT NOT NULL,
  revision INTEGER NOT NULL,
  pristine INTEGER NOT NULL CHECK (pristine IN (0, 1)),
  settings_json TEXT NOT NULL,
  api_key_nonce BLOB,
  api_key_ciphertext BLOB,
  updated_at INTEGER NOT NULL
);
