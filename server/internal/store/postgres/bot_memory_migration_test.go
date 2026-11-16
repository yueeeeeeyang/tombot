package postgres

import "testing"

// 验证已有聊天库能够前向升级，重复迁移不改正文且旧版本不能降级启动。
func TestBotMemoryForwardMigration(t *testing.T) {
	verifyMessageForwardMigration(t, "0068_")
}
