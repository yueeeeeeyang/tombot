package postgres

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestModelVersionStateForwardMigration(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, _ := cluster.Database(t)
	ctx := context.Background()
	previous := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0058_" {
			continue
		}
		encoded, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		previous["migrations/"+entry.Name()] = &fstest.MapFile{Data: encoded}
	}
	if err = migrate(ctx, pool, previous); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			t.Error(err)
		}
	})
	execute := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	provider, model, policy, stale := ids.New(), ids.New(), ids.New(), ids.New()
	execute(`INSERT INTO tombot.model_providers(id,name,protocol,base_url) VALUES($1,'迁移供应商','responses','https://models.example.test')`, provider)
	execute(`INSERT INTO tombot.models(id,provider_id,model_key,display_name,context_limit,output_limit,supports_tools,supports_vision) VALUES($1,$2,'state-model','迁移模型',4096,256,false,false)`, model, provider)
	for _, policyID := range []string{policy, stale} {
		execute(`INSERT INTO tombot.model_policies(id,name) VALUES($1,$1)`, policyID)
		for number := 1; number <= 4; number++ {
			execute(`INSERT INTO tombot.model_policy_versions(policy_id,version,routing_mode,status) VALUES($1,$2,'priority','draft')`, policyID, number)
			execute(`INSERT INTO tombot.model_policy_candidates(policy_id,version,position,provider_id,model_id,configuration,priority,weight) VALUES($1,$2,1,$3,$4,'{"protocol":"responses","base_url":"https://models.example.test","model_key":"state-model","context_limit":4096,"output_limit":256,"supports_tools":false,"supports_vision":false,"prices":{}}',1,1)`, policyID, number, provider, model)
			if number < 4 {
				execute(`UPDATE tombot.model_policy_versions SET status='published' WHERE policy_id=$1 AND version=$2`, policyID, number)
			}
		}
		chosen := 2
		if policyID == stale {
			chosen = 3
		}
		execute(`UPDATE tombot.model_policies SET current_version=$2,revision=revision+1 WHERE id=$1`, policyID, chosen)
		// 旧约束允许状态先变更、指针后清除，迁移须修复遗留的不一致。
		execute(`UPDATE tombot.model_policy_versions SET status='revoked' WHERE policy_id=$1 AND version=3`, policyID)
	}
	var verified time.Time
	if err = tx.QueryRowContext(ctx, `SELECT published_at FROM tombot.model_policy_versions WHERE policy_id=$1 AND version=2`, policy).Scan(&verified); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = Migrate(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	var current *int
	if err = pool.QueryRow(`SELECT current_version FROM tombot.model_policies WHERE id=$1`, policy).Scan(&current); err != nil || current == nil || *current != 2 {
		t.Fatal("迁移改变了有效的启用版本", err)
	}
	if err = pool.QueryRow(`SELECT current_version FROM tombot.model_policies WHERE id=$1`, stale).Scan(&current); err != nil || current != nil {
		t.Fatal("迁移未清理无效启用指针", err)
	}
	for number := 1; number <= 4; number++ {
		var timestamp *time.Time
		if err = pool.QueryRow(`SELECT verified_at FROM tombot.model_policy_versions WHERE policy_id=$1 AND version=$2`, policy, number).Scan(&timestamp); err != nil {
			t.Fatal(err)
		}
		if (number < 3) != (timestamp != nil) {
			t.Fatal("历史验证状态转换错误", number)
		}
		if number == 2 && !timestamp.Equal(verified) {
			t.Fatal("历史验证时间改变")
		}
	}
	for _, number := range []int{3, 4} {
		if _, err = pool.Exec(`UPDATE tombot.model_policies SET current_version=$2,revision=revision+1 WHERE id=$1`, policy, number); err == nil {
			t.Fatal("未验证版本可直接启用")
		}
	}
	if _, err = pool.Exec(`UPDATE tombot.model_policy_versions SET verified_at=clock_timestamp() WHERE policy_id=$1 AND version=3`, policy); err != nil {
		t.Fatal("原撤销版本无法重新验证", err)
	}
	if _, err = pool.Exec(`UPDATE tombot.model_policies SET current_version=3,revision=revision+1 WHERE id=$1`, policy); err != nil {
		t.Fatal("原撤销版本验证后无法启用", err)
	}
	if _, err = pool.Exec(`UPDATE tombot.model_policy_versions SET verified_at=NULL WHERE policy_id=$1 AND version=3`, policy); err == nil {
		t.Fatal("已验证历史可改写")
	}
	if _, err = pool.Exec(`UPDATE tombot.model_policies SET current_version=NULL,revision=revision+1 WHERE id=$1`, policy); err != nil {
		t.Fatal("启用版本无法停用", err)
	}
	if _, err = pool.Exec(`UPDATE tombot.model_policies SET current_version=1,revision=revision+1 WHERE id=$1`, policy); err != nil {
		t.Fatal("旧版本无法重新启用", err)
	}
	var legacy int
	if err = pool.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_schema='tombot' AND table_name='model_policy_versions' AND column_name IN ('status','published_at')`).Scan(&legacy); err != nil || legacy != 0 {
		t.Fatal("旧状态字段仍然存在", err)
	}
}
