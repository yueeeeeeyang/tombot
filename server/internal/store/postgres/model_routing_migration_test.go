package postgres

import (
	"context"
	"database/sql"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestMultiModelForwardMigration(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, _ := cluster.Database(t)
	ctx := context.Background()
	previous := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0057_" {
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
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	user, bot, desktop, node, image, computer, conversation, turn, run, operation := ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New()
	provider, model, policy := ids.New(), ids.New(), ids.New()
	config := `{"protocol":"responses","base_url":"https://models.example.test","model_key":"historical","context_limit":4096,"output_limit":256,"supports_tools":false,"supports_vision":false,"prices":{}}`
	exec(`INSERT INTO tombot.model_providers(id,name,protocol,base_url) VALUES($1,'旧供应商','responses','https://models.example.test')`, provider)
	exec(`INSERT INTO tombot.models(id,provider_id,model_key,display_name,context_limit,output_limit,supports_tools,supports_vision) VALUES($1,$2,'historical','旧模型',4096,1024,false,false)`, model, provider)
	exec(`INSERT INTO tombot.model_policies(id,name) VALUES($1,'旧策略')`, policy)
	exec(`INSERT INTO tombot.model_policy_versions(policy_id,version,provider_id,model_id,configuration,status) VALUES($1,1,$2,$3,$4,'draft')`, policy, provider, model, config)
	exec(`UPDATE tombot.model_policy_versions SET status='published' WHERE policy_id=$1 AND version=1`, policy)
	exec(`UPDATE tombot.model_policies SET current_version=1,revision=revision+1 WHERE id=$1`, policy)
	exec(`INSERT INTO tombot.users(id,username,display_name,status) VALUES($1,$2,'迁移用户','provisioning')`, user, strings.ToLower(user))
	exec(`INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, image, "example.test/image@sha256:"+strings.Repeat("a", 64))
	exec(`INSERT INTO tombot.computer_nodes(id,name,status,capacity) VALUES($1,$1,'bootstrapping',1)`, node)
	exec(`INSERT INTO tombot.computers(user_id,id,node_id,image_id,status,volume_ref,cpu_millis,memory_bytes,disk_bytes,process_limit) VALUES($1,$2,$3,$4,'pending',$2,1000,1073741824,1073741824,128)`, user, computer, node, image)
	exec(`INSERT INTO tombot.desktop_identity_heads(user_id,computer_id,shared_gid,last_display_number) VALUES($1,$2,20000,1)`, user, computer)
	exec(`INSERT INTO tombot.bots(user_id,id,desktop_id,name) VALUES($1,$2,$3,'迁移机器人')`, user, bot, desktop)
	exec(`INSERT INTO tombot.bot_desktops(user_id,id,bot_id,computer_id,linux_uid,shared_gid,display_number) VALUES($1,$2,$3,$4,100001,20000,1)`, user, desktop, bot, computer)
	exec(`INSERT INTO tombot.conversations(user_id,id,kind,bot_id,title) VALUES($1,$2,'direct',$3,'迁移会话')`, user, conversation, bot)
	exec(`INSERT INTO tombot.turns(user_id,id,conversation_id,sequence,client_request_id,request_digest,source,status) VALUES($1,$2,$3,1,$2,$4,'chat','running')`, user, turn, conversation, make([]byte, 32))
	exec(`INSERT INTO tombot.runs(user_id,id,turn_id,conversation_id,bot_id,bot_revision,bot_snapshot,model_policy_id,model_version,status) VALUES($1,$2,$3,$4,$5,1,'{}',$6,1,'running')`, user, run, turn, conversation, bot, policy)
	exec(`INSERT INTO tombot.run_operations(user_id,id,run_id,step_key,kind,request_digest,fencing_token,attempt,status) VALUES($1,$2,$3,'model.1','model',$4,1,1,'started')`, user, operation, run, make([]byte, 32))
	exec(`INSERT INTO tombot.usage_periods(user_id,period,held_tokens) VALUES($1,date_trunc('month',current_date)::date,4352)`, user)
	exec(`SELECT set_config('tombot.user_id',$1,true)`, user)
	exec(`INSERT INTO tombot.usage_reservations(user_id,id,run_id,bot_id,policy_id,model_version,period,fencing_token,attempt,reserved_tokens,held_tokens,prices,input_includes_cache,state,slot_expires_at)
 VALUES($1,$2,$3,$4,$5,1,date_trunc('month',current_date)::date,1,1,4352,4352,'{}',true,'active',clock_timestamp()+interval '1 minute')`, user, operation, run, bot, policy)
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = Migrate(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	var equal bool
	var mode, selected, metered string
	if err = pool.QueryRow(`SELECT c.configuration=$2::jsonb,v.routing_mode FROM tombot.model_policy_versions v JOIN tombot.model_policy_candidates c USING(policy_id,version) WHERE v.policy_id=$1`, policy, config).Scan(&equal, &mode); err != nil || !equal || mode != "priority" {
		t.Fatal("历史策略快照转换错误", err)
	}
	if err = pool.QueryRow(`SELECT model_id FROM tombot.model_routes WHERE user_id=$1 AND run_id=$2`, user, run).Scan(&selected); err != nil || selected != model {
		t.Fatal("在途运行未固定旧候选", err)
	}
	if err = pool.QueryRow(`SELECT model_id FROM tombot.usage_reservations WHERE user_id=$1 AND id=$2`, user, operation).Scan(&metered); err != nil || metered != model {
		t.Fatal("历史用量未关联原模型", err)
	}
	var legacy int
	if err = pool.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_schema='tombot' AND table_name='model_policy_versions' AND column_name IN ('model_id','provider_id','credential_id','configuration')`).Scan(&legacy); err != nil || legacy != 0 {
		t.Fatal("旧单模型结构残留", err)
	}
}
