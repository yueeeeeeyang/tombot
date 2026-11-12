package store

import (
	"context"
	"testing"
)

func TestCustomCatMigrationPreservesProfilesAndAddsHumanDeliveryOnce(t *testing.T) {
	db := databaseBefore(t, 28)
	insertCat(t, db, "custom", "custom", "猫咪", "已有介绍", "未分组", "calico", "", "")
	if _, err := db.Exec("UPDATE bots SET tool_policy_json=json_set(tool_policy_json,'$.allowed_tools',json('[\"message_bot\"]')) WHERE id='custom'"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Migrate(context.Background(), db); err != nil {
			t.Fatal(err)
		}
	}
	var avatar, description string
	var tools int
	if err := db.QueryRow("SELECT custom_avatar,description FROM bots WHERE id='custom'").Scan(&avatar, &description); err != nil || avatar != "" || description != "已有介绍" {
		t.Fatal("旧资料改变", err, avatar, description)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM bots,json_each(tool_policy_json,'$.allowed_tools') WHERE bots.id='custom' AND value='message_user'").Scan(&tools); err != nil || tools != 1 {
		t.Fatal("用户通道未加入或重复", tools, err)
	}
}
