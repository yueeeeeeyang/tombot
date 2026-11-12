package bots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computerdesktop"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func (fixture fixture) desktop(t *testing.T, userID, desktopID string) computer.DesktopIdentity {
	t.Helper()
	var result computer.DesktopIdentity
	err := fixture.tenant.Tenant(context.Background(), userID, func(transaction *sql.Tx) error {
		var err error
		result, err = computerdesktop.Lookup(context.Background(), transaction, userID, desktopID)
		return err
	})
	if err != nil {
		t.Fatal("读取已登记桌面失败", err)
	}
	return result
}

func TestBotDesktopCatalog(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("创建共享电脑内独立身份并在归档和重建后保持", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		request := CreateRequest{ids.New(), spec("稳定身份")}
		first, _, err := fixture.service.Create(ctx, fixture.a, request)
		if err != nil || first.DesktopID == "" || first.DesktopID == first.ID {
			t.Fatal("未返回独立桌面标识", err)
		}
		second := fixture.create(t, fixture.a, "并行桌面")
		foreign := fixture.create(t, fixture.b, "其他用户")
		one, two, other := fixture.desktop(t, fixture.a.UserID, first.DesktopID), fixture.desktop(t, fixture.a.UserID, second.DesktopID), fixture.desktop(t, fixture.b.UserID, foreign.DesktopID)
		if one.ComputerID != two.ComputerID || one.GID != two.GID || one.UID == two.UID || one.DisplayNumber == two.DisplayNumber || one.DesktopID == two.DesktopID || one.BotID != first.ID {
			t.Fatal("同电脑共享组或桌面身份错误")
		}
		if other.ComputerID == one.ComputerID || other.UID != one.UID || other.DisplayNumber != one.DisplayNumber {
			t.Fatal("身份分配未按用户电脑独立管理")
		}
		archived, err := fixture.service.SetArchived(ctx, fixture.a, first.ID, first.Revision, true)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := fixture.service.SetArchived(ctx, fixture.a, first.ID, archived.Revision, false)
		if err != nil || restored.DesktopID != first.DesktopID {
			t.Fatal("归档恢复改变桌面", err)
		}
		execute(t, fixture.pool, `UPDATE tombot.computers SET generation=generation+1 WHERE user_id=$1`, fixture.a.UserID)
		replayed, created, err := fixture.service.Create(ctx, fixture.a, request)
		if err != nil || created || replayed.DesktopID != first.DesktopID || fixture.desktop(t, fixture.a.UserID, first.DesktopID) != one {
			t.Fatal("重建或幂等重试改变身份", err)
		}
		err = fixture.tenant.Tenant(ctx, fixture.a.UserID, func(transaction *sql.Tx) error {
			repeated, err := computerdesktop.Register(ctx, transaction, fixture.a.UserID, first.ID, first.DesktopID)
			if err == nil && repeated != one {
				return errors.New("重复登记改变身份")
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		third := fixture.create(t, fixture.a, "后续桌面")
		if fixture.desktop(t, fixture.a.UserID, third.DesktopID).DisplayNumber != 3 {
			t.Fatal("重试消耗了编号")
		}
		encoded, err := json.Marshal(first)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"linux_uid", "shared_gid", "display_number", "computer_id"} {
			if _, exists := fields[field]; exists {
				t.Fatal("目录泄露内部运行映射", field)
			}
		}
	})
	t.Run("首次并发登记同时处理主键和复合唯一索引冲突", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		const registrations = 32
		start := make(chan struct{})
		var group sync.WaitGroup
		failures := make(chan error, registrations)
		results := make(chan computer.DesktopIdentity, registrations)
		for range registrations {
			group.Go(func() {
				<-start
				botID, desktopID := ids.New(), ids.New()
				err := fixture.tenant.Tenant(ctx, fixture.a.UserID, func(transaction *sql.Tx) error {
					if _, err := transaction.Exec(`INSERT INTO tombot.bots(user_id,id,desktop_id,name) VALUES($1,$2,$3,$2)`, fixture.a.UserID, botID, desktopID); err != nil {
						return err
					}
					registered, err := computerdesktop.Register(ctx, transaction, fixture.a.UserID, botID, desktopID)
					if err == nil {
						results <- registered
					}
					return err
				})
				failures <- err
			})
		}
		close(start)
		group.Wait()
		close(failures)
		close(results)
		for err := range failures {
			if err != nil {
				t.Fatal(err)
			}
		}
		numbers := make(map[int]bool)
		for result := range results {
			if numbers[result.DisplayNumber] || result.DisplayNumber < 1 || result.DisplayNumber > registrations {
				t.Fatal("并发编号冲突或跳号")
			}
			numbers[result.DisplayNumber] = true
		}
		if len(numbers) != registrations {
			t.Fatal("并发登记缺失")
		}
	})
	t.Run("跨用户读取和非法绑定被数据库拒绝", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		first, foreign := fixture.create(t, fixture.a, "本人"), fixture.create(t, fixture.b, "他人")
		other := fixture.desktop(t, fixture.b.UserID, foreign.DesktopID)
		for _, pair := range [][2]string{{fixture.a.UserID, foreign.DesktopID}, {fixture.b.UserID, foreign.DesktopID}} {
			err := fixture.tenant.Tenant(ctx, fixture.a.UserID, func(transaction *sql.Tx) error {
				_, err := computerdesktop.Lookup(ctx, transaction, pair[0], pair[1])
				return err
			})
			if !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("读到其他用户桌面", err)
			}
		}
		for _, query := range []struct {
			sql  string
			args []any
			code string
		}{
			{`INSERT INTO tombot.bots(user_id,id,desktop_id,name) VALUES($1,$2,$3,'缺少桌面')`, []any{fixture.a.UserID, ids.New(), ids.New()}, "23503"},
			{`INSERT INTO tombot.bots(user_id,id,desktop_id,name) VALUES($1,$2,$3,'盗用桌面')`, []any{fixture.a.UserID, ids.New(), foreign.DesktopID}, "23503"},
			{`UPDATE tombot.bots SET desktop_id=$2 WHERE user_id=$1`, []any{fixture.a.UserID, ids.New()}, "23514"},
			{`UPDATE tombot.desktop_identity_heads SET last_display_number=0 WHERE user_id=$1`, []any{fixture.a.UserID}, "23514"},
			{`INSERT INTO tombot.bot_desktops(user_id,id,bot_id,computer_id,linux_uid,shared_gid,display_number) VALUES($1,$2,$3,$4,$5,$6,$7)`, []any{other.UserID, ids.New(), other.BotID, other.ComputerID, other.UID, other.GID, other.DisplayNumber}, "42501"},
		} {
			err := fixture.tenant.Tenant(ctx, fixture.a.UserID, func(transaction *sql.Tx) error {
				if _, err := transaction.Exec(query.sql, query.args...); err != nil {
					return err
				}
				// 提前触发延迟外键，以便直接核对数据库错误码；提交时也会执行同一约束。
				_, err := transaction.Exec(`SET CONSTRAINTS ALL IMMEDIATE`)
				return err
			})
			var failure *pgconn.PgError
			if !errors.As(err, &failure) || failure.Code != query.code {
				t.Fatal("身份约束未按预期拒绝", query.code, err)
			}
		}
		for _, query := range []string{`UPDATE tombot.bot_desktops SET linux_uid=0 WHERE user_id=$1`, `DELETE FROM tombot.bot_desktops WHERE user_id=$1`, `DELETE FROM tombot.desktop_identity_heads WHERE user_id=$1`} {
			_, err := fixture.pool.Exec(query, fixture.a.UserID)
			var failure *pgconn.PgError
			if !errors.As(err, &failure) || failure.Code != "23514" {
				t.Fatal("持久身份可被改写或回收", err)
			}
		}
		if fixture.desktop(t, fixture.a.UserID, first.DesktopID).BotID != first.ID {
			t.Fatal("拒绝后身份改变")
		}
	})
	t.Run("事件失败和身份耗尽整体回滚", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		execute(t, fixture.pool, `CREATE FUNCTION tombot.reject_desktop_event() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN RAISE EXCEPTION '事件写入失败'; END $body$`)
		execute(t, fixture.pool, `CREATE TRIGGER reject_desktop_event BEFORE INSERT ON tombot.user_events FOR EACH ROW EXECUTE FUNCTION tombot.reject_desktop_event()`)
		request := CreateRequest{ids.New(), spec("原子登记")}
		if _, _, err := fixture.service.Create(ctx, fixture.a, request); err == nil {
			t.Fatal("事件失败被忽略")
		}
		for _, table := range []string{"bots", "bot_desktops", "desktop_identity_heads", "conversations", "user_workspaces", "user_events"} {
			var count int
			if err := fixture.pool.QueryRow(`SELECT count(*) FROM tombot.`+table+` WHERE user_id=$1`, fixture.a.UserID).Scan(&count); err != nil || count != 0 {
				t.Fatal("失败遗留部分记录", table, count, err)
			}
		}
		execute(t, fixture.pool, `DROP TRIGGER reject_desktop_event ON tombot.user_events`)
		first, created, err := fixture.service.Create(ctx, fixture.a, request)
		if err != nil || !created || fixture.desktop(t, fixture.a.UserID, first.DesktopID).DisplayNumber != 1 {
			t.Fatal("失败重试不完整", err)
		}
		execute(t, fixture.pool, `UPDATE tombot.desktop_identity_heads SET last_display_number=$2 WHERE user_id=$1`, fixture.a.UserID, computer.MaxDesktops)
		_, _, err = fixture.service.Create(ctx, fixture.a, CreateRequest{ids.New(), spec("耗尽")})
		expect(t, err, ErrCapacity)
		var count int
		if err = fixture.pool.QueryRow(`SELECT count(*) FROM tombot.bots WHERE user_id=$1`, fixture.a.UserID).Scan(&count); err != nil || count != 1 {
			t.Fatal("身份耗尽留下机器人", err)
		}
	})
	t.Run("分配锁等待取消不遗留机器人", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		fixture.create(t, fixture.a, "持锁前")
		locked, err := fixture.pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := locked.Rollback(); err != nil {
				t.Error(err)
			}
		}()
		if _, err = locked.Exec(`SELECT 1 FROM tombot.desktop_identity_heads WHERE user_id=$1 FOR UPDATE`, fixture.a.UserID); err != nil {
			t.Fatal(err)
		}
		deadline, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		_, _, err = fixture.service.Create(deadline, fixture.a, CreateRequest{ids.New(), spec("不应遗留")})
		if err == nil {
			t.Fatal("忽略锁等待取消")
		}
		var count int
		if err = fixture.pool.QueryRow(`SELECT count(*) FROM tombot.bots WHERE user_id=$1`, fixture.a.UserID).Scan(&count); err != nil || count != 1 {
			t.Fatal("取消遗留机器人", err)
		}
	})
}
