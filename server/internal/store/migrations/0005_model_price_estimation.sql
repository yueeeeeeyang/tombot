-- 显式区分用户配置的免费价格和缺少价格时的估算状态。
ALTER TABLE models ADD COLUMN is_price_estimated INTEGER NOT NULL DEFAULT 1;
