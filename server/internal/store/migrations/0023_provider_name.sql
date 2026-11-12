-- 自定义提供商的显示名称。内置提供商不靠这一列决定接口地址。
ALTER TABLE workspace_meta ADD COLUMN provider_name TEXT NOT NULL DEFAULT '';
