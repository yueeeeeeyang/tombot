-- 许可限定到标签与完整源，关闭标签后级联清理；拒绝规则始终先于已有许可。
CREATE TABLE browser_origin_permissions (
  tab_id TEXT NOT NULL REFERENCES browser_tabs(id) ON DELETE CASCADE,
  origin TEXT NOT NULL CHECK(length(origin) BETWEEN 1 AND 2048),
  created_at INTEGER NOT NULL,
  PRIMARY KEY(tab_id, origin)
);
-- 只保存经过 URL 校验的被阻断目标，不记录请求正文、头部或凭据。
ALTER TABLE browser_tabs ADD COLUMN blocked_url TEXT NOT NULL DEFAULT '' CHECK(length(blocked_url)<=8192);
ALTER TABLE browser_tabs ADD COLUMN blocked_method TEXT NOT NULL DEFAULT '' CHECK(length(blocked_method)<=16);
ALTER TABLE browser_tabs ADD COLUMN blocked_code TEXT NOT NULL DEFAULT '' CHECK(length(blocked_code)<=128);
