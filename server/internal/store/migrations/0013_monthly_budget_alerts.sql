-- 每个 UTC 月份的阈值与上限各提醒一次，重启及并发结算不会重复通知。
CREATE TABLE monthly_budget_alerts (
  month      TEXT NOT NULL,
  level      TEXT NOT NULL CHECK(level IN ('warning','limit')),
  created_at INTEGER NOT NULL,
  PRIMARY KEY(month, level)
);
