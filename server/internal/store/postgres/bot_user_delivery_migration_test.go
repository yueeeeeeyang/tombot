package postgres

import "testing"

func TestBotUserDeliveryForwardMigration(t *testing.T) { verifyMessageForwardMigration(t, "0073_") }
