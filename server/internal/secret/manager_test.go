package secret

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/testkit"
)

func TestSecretCiphertextRotationAuditAndMemoryClear(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	key, err := secure.Random(32)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(db, key)
	plaintext := testkit.Random(t)
	record, err := manager.Create(ctx, "provider-test", plaintext, "user")
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext []byte
	if err = db.QueryRow("SELECT ciphertext FROM secrets WHERE id=?", record.ID).Scan(&ciphertext); err != nil || strings.Contains(string(ciphertext), plaintext) {
		t.Fatal("密文存储失败")
	}
	records, err := manager.List(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(records)
	if err != nil || strings.Contains(string(encoded), plaintext) {
		t.Fatal("元数据泄漏明文")
	}
	var retained []byte
	if err = manager.Use(ctx, record.ID, "user", "run", func(value []byte) error {
		retained = value
		if string(value) != plaintext {
			t.Fatal("解密不一致")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, b := range retained {
		if b != 0 {
			t.Fatal("执行后明文未清零")
		}
	}
	replacement := testkit.Random(t)
	if err = manager.Rotate(ctx, record.ID, replacement, "user"); err != nil {
		t.Fatal(err)
	}
	if err = manager.Use(ctx, record.ID, "user", "run", func(value []byte) error {
		if string(value) != replacement {
			t.Fatal("密钥未轮换")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var details string
	if err = db.QueryRow("SELECT COALESCE(GROUP_CONCAT(payload_json),'') FROM events").Scan(&details); err != nil || strings.Contains(details, plaintext) || strings.Contains(details, replacement) {
		t.Fatal("事件泄漏明文")
	}
	wrong, err := secure.Random(32)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if err = New(db, wrong).Use(ctx, record.ID, "user", "run", func([]byte) error { called = true; return nil }); err == nil || called {
		t.Fatal("错误主密钥被接受")
	}
}
func TestReferencedSecretCannotBeDeleted(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	key, err := secure.Random(32)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(db, key)
	record, err := manager.Create(ctx, "provider-test", testkit.Random(t), "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO providers(id,kind,display_name,secret_id,created_at) VALUES('p','openai_compat','测试',?,0)`, record.ID); err != nil {
		t.Fatal(err)
	}
	if err = manager.Delete(ctx, record.ID, "user"); err == nil {
		t.Fatal("引用中的密钥可被删除")
	}
}
