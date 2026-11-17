package postgres

import "testing"

// 新迁移保留已有消息并允许独立群聊回合。
func TestGroupChatForwardMigration(t *testing.T) { verifyMessageForwardMigration(t, "0070_") }
