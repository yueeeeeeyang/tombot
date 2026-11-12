package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestLastAdministratorAndEnrollmentOwnership(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, _ := cluster.Database(t)
	if err := postgres.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	fixture := identityFixture{pool: pool}
	first, second := fixture.pending(t, "admin"), fixture.pending(t, "admin")
	for _, account := range []testAccount{first, second} {
		execute(t, pool, `UPDATE tombot.users SET status='active' WHERE id=$1`, account.id)
	}
	results := make(chan error, 2)
	for _, account := range []testAccount{first, second} {
		go func() {
			_, err := pool.Exec(`UPDATE tombot.users SET status='disabled' WHERE id=$1`, account.id)
			results <- err
		}()
	}
	success := 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
			continue
		}
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || databaseError.Code != "23514" {
			t.Fatal("最后管理员保护返回了意外错误", err)
		}
	}
	if success != 1 {
		t.Fatal("并发停用没有保护最后管理员", success)
	}
	var active string
	if err := pool.QueryRow(`SELECT id FROM tombot.users WHERE role='admin' AND status='active'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`UPDATE tombot.users SET role='user' WHERE id=$1`, `DELETE FROM tombot.users WHERE id=$1`} {
		_, err := pool.Exec(query, active)
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || databaseError.Code != "23514" {
			t.Fatal("最后管理员降权或删除没有被阻止", err)
		}
	}
	// 两个合法用户和激活记录不能任意交叉组成多因素材料的所属关系。
	_, err := pool.Exec(`INSERT INTO tombot.activation_mfa(user_id,activation_id,ciphertext,nonce)
		SELECT $1,id,decode(repeat('00',32),'hex'),decode(repeat('00',12),'hex') FROM tombot.activation_tokens WHERE user_id=$2`, first.id, second.id)
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError.Code != "23503" {
		t.Fatal("验证材料跨账户关联未被外键阻止", err)
	}
}
