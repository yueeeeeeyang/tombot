package postgres

import "testing"

func TestBotPrivateForwardMigration(t *testing.T) { verifyMessageForwardMigration(t, "0071_") }
