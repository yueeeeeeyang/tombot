package postgres

import (
	"database/sql"
	"encoding/json"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestDefaultComputerToolsForwardMigration(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	previous := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0075_" {
			continue
		}
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		previous["migrations/"+entry.Name()] = &fstest.MapFile{Data: content}
	}
	execute := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, role := range []Role{TenantRole, IdentityRole, ControlRole, DispatchRole} {
		execute(`DO $roles$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='` + string(role) + `') THEN CREATE ROLE ` + string(role) + ` NOLOGIN; END IF; END $roles$`)
	}
	owner := cluster.Login(t)
	execute(`GRANT CREATE ON DATABASE ` + name + ` TO ` + owner)
	migration := pgtest.Connect(t, cluster.DSN(name, owner))
	if err := migrate(t.Context(), migration, previous); err != nil {
		t.Fatal(err)
	}
	const prompt = "You are a helpful assistant. Collaborate with the user and use the available tools when needed."
	fullTools := []string{"computer_browser", "computer_input", "computer_screenshot", "computer_shell"}
	type stored struct {
		user, bot, desktop string
		tools              []string
		changed            bool
	}
	var records []stored
	for range 2 {
		user, computer, node, image := ids.New(), ids.New(), ids.New(), ids.New()
		execute(`INSERT INTO tombot.users(id,username,display_name,status) VALUES($1,$2,'迁移测试用户','provisioning')`, user, strings.ToLower(user))
		execute(`INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, image, "example.test/image@sha256:"+secure.Hash(image))
		execute(`INSERT INTO tombot.computer_nodes(id,name,status,capacity) VALUES($1,$1,'bootstrapping',1)`, node)
		execute(`INSERT INTO tombot.computers(user_id,id,node_id,image_id,status,volume_ref,cpu_millis,memory_bytes,disk_bytes,process_limit) VALUES($1,$2,$3,$4,'pending',$2,1000,1073741824,1073741824,128)`, user, computer, node, image)
		execute(`INSERT INTO tombot.desktop_identity_heads(user_id,computer_id,shared_gid,last_display_number) VALUES($1,$2,20000,6)`, user, computer)
		for index, fixture := range []struct {
			tools             []string
			prompt            string
			archived, changed bool
		}{
			{[]string{"computer_shell"}, prompt, false, true},
			{[]string{"computer_shell"}, prompt, true, true},
			{[]string{}, prompt, false, false},
			{[]string{"computer_shell"}, "You are a specialized assistant.", false, false},
			{[]string{"computer_shell", "computer_browser"}, prompt, false, false},
			{fullTools, prompt, false, false},
		} {
			bot, desktop := ids.New(), ids.New()
			encoded, err := json.Marshal(fixture.tools)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := pool.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
					t.Error(err)
				}
			})
			if _, err = tx.Exec(`INSERT INTO tombot.bots(user_id,id,desktop_id,name,system_prompt,tool_permissions,is_archived,revision) VALUES($1,$2,$3,$2,$4,$5,$6,7)`, user, bot, desktop, fixture.prompt, encoded, fixture.archived); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(`INSERT INTO tombot.bot_desktops(user_id,id,bot_id,computer_id,linux_uid,shared_gid,display_number) VALUES($1,$2,$3,$4,$5,20000,$6)`, user, desktop, bot, computer, 100001+index, index+1); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			records = append(records, stored{user, bot, desktop, fixture.tools, fixture.changed})
		}
	}
	verify := func(upgraded bool) {
		t.Helper()
		for _, record := range records {
			var raw []byte
			var revision int
			var desktop string
			if err := pool.QueryRow(`SELECT tool_permissions,revision,desktop_id FROM tombot.bots WHERE user_id=$1 AND id=$2`, record.user, record.bot).Scan(&raw, &revision, &desktop); err != nil {
				t.Fatal(err)
			}
			var actual []string
			if err := json.Unmarshal(raw, &actual); err != nil {
				t.Fatal(err)
			}
			want, wantRevision := record.tools, 7
			if upgraded && record.changed {
				want, wantRevision = fullTools, 8
			}
			if !slices.Equal(actual, want) || revision != wantRevision || desktop != record.desktop {
				t.Fatal("权限、修订或桌面身份迁移错误", actual, revision)
			}
		}
		var policies int
		if err := pool.QueryRow(`SELECT count(*) FROM pg_policies WHERE schemaname='tombot' AND policyname='default_computer_tools_migration'`).Scan(&policies); err != nil || policies != 0 {
			t.Fatal("临时迁移权限未回收", err)
		}
	}
	execute(`CREATE FUNCTION tombot.reject_default_tools_for_test() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN RAISE EXCEPTION '测试更新失败'; END $body$`)
	execute(`CREATE TRIGGER reject_default_tools_for_test BEFORE UPDATE ON tombot.bots FOR EACH ROW EXECUTE FUNCTION tombot.reject_default_tools_for_test()`)
	if err := Migrate(t.Context(), migration); err == nil {
		t.Fatal("更新失败未阻止迁移")
	}
	verify(false)
	var version int
	if err := pool.QueryRow(`SELECT max(version) FROM tombot.schema_migrations`).Scan(&version); err != nil || version != 74 {
		t.Fatal("失败迁移写入了版本账本", err)
	}
	execute(`DROP TRIGGER reject_default_tools_for_test ON tombot.bots`)
	execute(`DROP FUNCTION tombot.reject_default_tools_for_test()`)
	for range 2 {
		if err := Migrate(t.Context(), migration); err != nil {
			t.Fatal(err)
		}
		verify(true)
	}
	if err := migrate(t.Context(), migration, previous); err == nil {
		t.Fatal("旧程序允许降级启动")
	}
}
