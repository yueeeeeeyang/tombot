package postgres

import "testing"

// 已有消息数据库只能通过新迁移接入任务；重复启动不改历史迁移或正文。
func TestScheduledTasksForwardMigration(t *testing.T) { verifyMessageForwardMigration(t, "0069_") }
