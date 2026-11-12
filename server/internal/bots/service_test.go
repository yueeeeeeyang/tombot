package bots

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"image"
	"image/png"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"io.yueyang/tom/internal/identity"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

type fixture struct {
	pool    *sql.DB
	tenant  *postgres.Database
	service *Service
	a, b    identity.Principal
}

func execute(t *testing.T, pool *sql.DB, statement string, arguments ...any) {
	t.Helper()
	if _, err := pool.Exec(statement, arguments...); err != nil {
		t.Fatal("准备目录测试记录失败", err)
	}
}
func newFixture(t *testing.T, cluster *pgtest.Cluster) fixture {
	t.Helper()
	pool, name := cluster.Database(t)
	if err := postgres.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	login := cluster.Login(t, string(postgres.TenantRole))
	tenant, err := postgres.Open(context.Background(), postgres.Options{DSN: cluster.DSN(name, login), Role: postgres.TenantRole, MaxConnections: 8, ConnectTimeout: 5 * time.Second, StatementTimeout: 5 * time.Second, LockTimeout: 2 * time.Second, TransactionTimeout: 10 * time.Second, ConnectionLifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tenant.Close(); err != nil {
			t.Error(err)
		}
	})
	service, err := New(tenant, Options{MaxBots: 20, MaxGroups: 10, ToolNames: []string{"file_read", "send_user_message", "send_bot_message"}})
	if err != nil {
		t.Fatal(err)
	}
	seed := func() identity.Principal {
		principal := identity.Principal{UserID: ids.New(), SessionID: ids.New(), DeviceID: ids.New(), Role: "user", Scope: "desktop", AuthVersion: 1}
		node, image, computer := ids.New(), ids.New(), ids.New()
		execute(t, pool, `INSERT INTO tombot.users(id,username,display_name,role,status) VALUES($1,$2,'目录用户','user','provisioning')`, principal.UserID, strings.ToLower(principal.UserID))
		execute(t, pool, `INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, image, "example.test/computer@sha256:"+secure.Hash(image))
		execute(t, pool, `INSERT INTO tombot.computer_nodes(id,name,status,capacity,runtime_id,control_sequence,last_seen_at,control_expires_at,verified_image_id,capability_expires_at) VALUES($1,$1,'ready',1,$1,1,clock_timestamp(),clock_timestamp()+interval '5 minutes',$2,clock_timestamp()+interval '15 minutes')`, node, image)
		execute(t, pool, `INSERT INTO tombot.computers(user_id,id,node_id,image_id,status,container_id,volume_ref,cpu_millis,memory_bytes,disk_bytes,process_limit) VALUES($1,$2,$3,$4,'ready',$2,$2,1000,1073741824,1073741824,128)`, principal.UserID, computer, node, image)
		execute(t, pool, `UPDATE tombot.users SET status='active' WHERE id=$1`, principal.UserID)
		execute(t, pool, `INSERT INTO tombot.devices(user_id,id,name,platform) VALUES($1,$2,'测试设备','linux')`, principal.UserID, principal.DeviceID)
		execute(t, pool, `INSERT INTO tombot.user_sessions(user_id,id,device_id,auth_version,scope,expires_at) VALUES($1,$2,$3,1,'desktop',clock_timestamp()+interval '1 hour')`, principal.UserID, principal.SessionID, principal.DeviceID)
		return principal
	}
	return fixture{pool, tenant, service, seed(), seed()}
}
func spec(name string) Spec {
	return Spec{Name: name, Description: "整理信息与协作", SystemPrompt: "You are a helpful bot. Verify facts and respect the user's instructions.", ToolPermissions: []string{"file_read", "send_user_message"}, ApprovalMode: "standard", MessagingPolicy: MessagingPolicy{true, true, true}, AccentHue: 120}
}
func (fixture fixture) create(t *testing.T, principal identity.Principal, name string) Bot {
	t.Helper()
	bot, created, err := fixture.service.Create(context.Background(), principal, CreateRequest{ids.New(), spec(name)})
	if err != nil || !created {
		t.Fatal("创建机器人失败", err)
	}
	return bot
}
func expect(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("预期 %v，实际 %v", want, err)
	}
}
func avatarPNG(t *testing.T, size int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, size, size))); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestBotCatalogIntegration(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("幂等创建与用户隔离", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		request := CreateRequest{ids.New(), spec("研究员")}
		bot, created, err := fixture.service.Create(ctx, fixture.a, request)
		if err != nil || !created || bot.Revision != 1 || bot.ConversationID == "" {
			t.Fatal("新建目录不完整", err)
		}
		replayed, created, err := fixture.service.Create(ctx, fixture.a, request)
		if err != nil || created || replayed.ID != bot.ID {
			t.Fatal("幂等键重复创建", err)
		}
		request.Description = "另一项任务"
		_, _, err = fixture.service.Create(ctx, fixture.a, request)
		expect(t, err, ErrConflict)
		request.ClientRequestID = ids.New()
		_, _, err = fixture.service.Create(ctx, fixture.a, request)
		expect(t, err, ErrConflict)
		other := fixture.create(t, fixture.b, "研究员")
		_, err = fixture.service.Get(ctx, fixture.a, other.ID)
		expect(t, err, ErrNotFound)
		_, err = fixture.service.GetConversation(ctx, fixture.a, other.ConversationID)
		expect(t, err, ErrNotFound)
		_, err = fixture.service.Update(ctx, fixture.a, other.ID, UpdateRequest{1, spec("窜改")})
		expect(t, err, ErrNotFound)
		snapshot, err := fixture.service.Snapshot(ctx, fixture.a, 10)
		if err != nil || len(snapshot.Bots) != 1 || snapshot.Bots[0].ID != bot.ID || snapshot.EventSequence != 2 || snapshot.Workspace.ActiveBotID == nil || *snapshot.Workspace.ActiveBotID != bot.ID || snapshot.Workspace.PrimaryBotID == nil || *snapshot.Workspace.PrimaryBotID != bot.ID {
			t.Fatal("工作空间与事件不一致", err)
		}
		conversation, err := fixture.service.GetConversation(ctx, fixture.a, bot.ConversationID)
		if err != nil || conversation.Kind != "direct" || conversation.BotID == nil || *conversation.BotID != bot.ID || conversation.MessageSequence != 0 {
			t.Fatal("唯一单聊错误", err)
		}
		_, err = fixture.service.UpdateWorkspace(ctx, fixture.a, WorkspaceRequest{snapshot.Workspace.Revision, &other.ID, nil})
		expect(t, err, ErrNotFound)
		var count int
		if err = fixture.pool.QueryRow(`SELECT count(*) FROM tombot.bots WHERE user_id=$1`, fixture.a.UserID).Scan(&count); err != nil || count != 1 {
			t.Fatal("失败创建遗留资源", err)
		}
	})
	t.Run("分页修订和归档恢复", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		first := fixture.create(t, fixture.a, "一号")
		second := fixture.create(t, fixture.a, "二号")
		page, err := fixture.service.List(ctx, fixture.a, "", 1, false)
		if err != nil || len(page) != 1 || page[0].ID != second.ID {
			t.Fatal("首页排序错误", err)
		}
		page, err = fixture.service.List(ctx, fixture.a, page[0].ID, 1, false)
		if err != nil || len(page) != 1 || page[0].ID != first.ID {
			t.Fatal("后续分页错误", err)
		}
		updated, err := fixture.service.Update(ctx, fixture.a, first.ID, UpdateRequest{first.Revision, spec("新名称")})
		if err != nil || updated.Revision != 2 {
			t.Fatal(err)
		}
		_, err = fixture.service.Update(ctx, fixture.a, first.ID, UpdateRequest{first.Revision, spec("迟到的名称")})
		expect(t, err, ErrConflict)
		conversation, err := fixture.service.GetConversation(ctx, fixture.a, first.ConversationID)
		if err != nil || conversation.Title != "新名称" {
			t.Fatal("会话标题未同步", err)
		}
		archived, err := fixture.service.SetArchived(ctx, fixture.a, first.ID, updated.Revision, true)
		if err != nil || !archived.IsArchived {
			t.Fatal(err)
		}
		snapshot, err := fixture.service.Snapshot(ctx, fixture.a, 1)
		if err != nil || len(snapshot.Bots) != 1 || snapshot.Workspace.ActiveBotID != nil || snapshot.Workspace.PrimaryBotID != nil {
			t.Fatal("归档选择未清理", err)
		}
		_, err = fixture.service.Update(ctx, fixture.a, first.ID, UpdateRequest{archived.Revision, spec("仍然修改")})
		expect(t, err, ErrArchived)
		fixture.create(t, fixture.a, "新名称")
		_, err = fixture.service.SetArchived(ctx, fixture.a, first.ID, archived.Revision, false)
		expect(t, err, ErrConflict)
		conversation, err = fixture.service.GetConversation(ctx, fixture.a, first.ConversationID)
		if err != nil || !conversation.IsArchived {
			t.Fatal("恢复冲突仍修改会话", err)
		}
		snapshot, err = fixture.service.Snapshot(ctx, fixture.a, 1)
		if err != nil || !snapshot.HasMoreBots {
			t.Fatal("目录截断未告知", err)
		}
		conversations, err := fixture.service.ListConversations(ctx, fixture.a, "", 10, true)
		if err != nil || len(conversations) != 1 || conversations[0].ID != first.ConversationID {
			t.Fatal("归档会话不可读取", err)
		}
	})
	t.Run("并发幂等与修订竞争", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		request := CreateRequest{ids.New(), spec("并发创建")}
		var group sync.WaitGroup
		results := make(chan Bot, 8)
		failures := make(chan error, 8)
		for range 8 {
			group.Go(func() {
				bot, _, err := fixture.service.Create(ctx, fixture.a, request)
				results <- bot
				failures <- err
			})
		}
		group.Wait()
		close(results)
		close(failures)
		for err := range failures {
			if err != nil {
				t.Fatal(err)
			}
		}
		var id, desktopID string
		for bot := range results {
			if bot.DesktopID == "" || id != "" && (bot.ID != id || bot.DesktopID != desktopID) {
				t.Fatal("同键产生多个机器人或桌面")
			}
			id, desktopID = bot.ID, bot.DesktopID
		}
		failures = make(chan error, 2)
		for _, name := range []string{"修订甲", "修订乙"} {
			group.Go(func() {
				_, err := fixture.service.Update(ctx, fixture.a, id, UpdateRequest{1, spec(name)})
				failures <- err
			})
		}
		group.Wait()
		close(failures)
		succeeded, conflicted := 0, 0
		for err := range failures {
			if err == nil {
				succeeded++
			} else if errors.Is(err, ErrConflict) {
				conflicted++
			} else {
				t.Fatal(err)
			}
		}
		if succeeded != 1 || conflicted != 1 {
			t.Fatal("并发修改覆盖", succeeded, conflicted)
		}
	})
	t.Run("群组成员与会话原子更新", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		first := fixture.create(t, fixture.a, "甲")
		second := fixture.create(t, fixture.a, "乙")
		foreign := fixture.create(t, fixture.b, "他人")
		request := CreateGroupRequest{ids.New(), GroupSpec{Name: "工作组", MemberIDs: []string{second.ID, first.ID}}}
		group, created, err := fixture.service.CreateGroup(ctx, fixture.a, request)
		if err != nil || !created || len(group.MemberIDs) != 2 {
			t.Fatal("创建群组失败", err)
		}
		replayed, created, err := fixture.service.CreateGroup(ctx, fixture.a, request)
		if err != nil || created || replayed.ID != group.ID {
			t.Fatal("群组重试不幂等", err)
		}
		_, err = fixture.service.GetGroup(ctx, fixture.b, group.ID)
		expect(t, err, ErrNotFound)
		_, err = fixture.service.UpdateGroup(ctx, fixture.a, group.ID, UpdateGroupRequest{group.Revision, GroupSpec{Name: "跨用户", MemberIDs: []string{first.ID, foreign.ID}}})
		expect(t, err, ErrNotFound)
		unchanged, err := fixture.service.GetGroup(ctx, fixture.a, group.ID)
		if err != nil || unchanged.Name != group.Name || unchanged.Revision != group.Revision || len(unchanged.MemberIDs) != 2 {
			t.Fatal("失败更新破坏成员", err)
		}
		changed := second.Spec
		changed.MessagingPolicy.AcceptGroupInvite = false
		if _, err = fixture.service.Update(ctx, fixture.a, second.ID, UpdateRequest{second.Revision, changed}); err != nil {
			t.Fatal(err)
		}
		_, _, err = fixture.service.CreateGroup(ctx, fixture.a, CreateGroupRequest{ids.New(), GroupSpec{Name: "拒绝邀请", MemberIDs: []string{second.ID}}})
		expect(t, err, ErrInviteDenied)
		group, err = fixture.service.UpdateGroup(ctx, fixture.a, group.ID, UpdateGroupRequest{group.Revision, GroupSpec{Name: "继续工作", MemberIDs: group.MemberIDs}})
		if err != nil {
			t.Fatal("现有成员不应被邀请设置移除", err)
		}
		conversation, err := fixture.service.GetConversation(ctx, fixture.a, group.ConversationID)
		if err != nil || conversation.Title != "继续工作" || conversation.GroupID == nil || *conversation.GroupID != group.ID {
			t.Fatal("群会话错误", err)
		}
		group, err = fixture.service.SetGroupArchived(ctx, fixture.a, group.ID, group.Revision, true)
		if err != nil || !group.IsArchived {
			t.Fatal(err)
		}
		_, err = fixture.service.UpdateGroup(ctx, fixture.a, group.ID, UpdateGroupRequest{group.Revision, group.GroupSpec})
		expect(t, err, ErrArchived)
		groups, err := fixture.service.ListGroups(ctx, fixture.a, "", 10, true)
		if err != nil || len(groups) != 1 || groups[0].ID != group.ID {
			t.Fatal("归档群组错误", err)
		}
		newGroup, _, err := fixture.service.CreateGroup(ctx, fixture.a, CreateGroupRequest{ids.New(), GroupSpec{Name: group.Name, MemberIDs: []string{first.ID}}})
		if err != nil {
			t.Fatal("归档名称未释放", err)
		}
		_, _, err = fixture.service.CreateGroup(ctx, fixture.a, CreateGroupRequest{ids.New(), GroupSpec{Name: group.Name, MemberIDs: []string{first.ID}}})
		expect(t, err, ErrConflict)
		_, err = fixture.service.SetGroupArchived(ctx, fixture.a, group.ID, group.Revision, false)
		expect(t, err, ErrConflict)
		fixture.service.options.MaxGroups = 2
		_, _, err = fixture.service.CreateGroup(ctx, fixture.a, CreateGroupRequest{ids.New(), GroupSpec{Name: "超出群组容量", MemberIDs: []string{first.ID}}})
		expect(t, err, ErrCapacity)
		if _, err = fixture.service.SetGroupArchived(ctx, fixture.a, newGroup.ID, newGroup.Revision, true); err != nil {
			t.Fatal(err)
		}
		if _, err = fixture.service.SetGroupArchived(ctx, fixture.a, group.ID, group.Revision, false); err != nil {
			t.Fatal("无名称冲突时恢复失败", err)
		}
	})
	t.Run("自定义头像与完整性", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		bot := fixture.create(t, fixture.a, "自定义资料")
		content := avatarPNG(t, 128)
		updated, err := fixture.service.PutAvatar(ctx, fixture.a, bot.ID, bot.Revision, append(content, []byte("trailing metadata")...))
		if err != nil || !updated.HasCustomAvatar || updated.Revision != bot.Revision+1 {
			t.Fatal("头像未关联资源修订", err)
		}
		avatar, err := fixture.service.GetAvatar(ctx, fixture.a, bot.ID)
		if err != nil || !bytes.Equal(avatar.Content, content) || len(avatar.SHA256) != 64 {
			t.Fatal("头像未规范化", err)
		}
		_, err = fixture.service.GetAvatar(ctx, fixture.b, bot.ID)
		expect(t, err, ErrNotFound)
		_, err = fixture.service.PutAvatar(ctx, fixture.b, bot.ID, updated.Revision, content)
		expect(t, err, ErrNotFound)
		_, err = fixture.service.PutAvatar(ctx, fixture.a, bot.ID, bot.Revision, content)
		expect(t, err, ErrConflict)
		updated, err = fixture.service.RemoveAvatar(ctx, fixture.a, bot.ID, updated.Revision)
		if err != nil || updated.HasCustomAvatar {
			t.Fatal(err)
		}
		_, err = fixture.service.GetAvatar(ctx, fixture.a, bot.ID)
		expect(t, err, ErrNotFound)
		for _, invalid := range [][]byte{nil, []byte("<svg/>"), avatarPNG(t, 129), make([]byte, MaxAvatarBytes+1)} {
			_, err = fixture.service.PutAvatar(ctx, fixture.a, bot.ID, updated.Revision, invalid)
			expect(t, err, ErrInvalidInput)
		}
		updated, err = fixture.service.PutAvatar(ctx, fixture.a, bot.ID, updated.Revision, content)
		if err != nil {
			t.Fatal(err)
		}
		execute(t, fixture.pool, `UPDATE tombot.bot_avatars SET content=$3 WHERE user_id=$1 AND bot_id=$2`, fixture.a.UserID, bot.ID, []byte("broken"))
		if _, err = fixture.service.GetAvatar(ctx, fixture.a, bot.ID); err == nil {
			t.Fatal("损坏头像被返回")
		}
	})
	t.Run("事件失败回滚与容量", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		fixture.service.options.MaxBots = 1
		bot := fixture.create(t, fixture.a, "容量内")
		_, _, err := fixture.service.Create(ctx, fixture.a, CreateRequest{ids.New(), spec("超限")})
		expect(t, err, ErrCapacity)
		execute(t, fixture.pool, `CREATE FUNCTION tombot.reject_catalog_event() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN RAISE EXCEPTION '事件写入失败'; END $body$`)
		execute(t, fixture.pool, `CREATE TRIGGER reject_catalog_event BEFORE INSERT ON tombot.user_events FOR EACH ROW EXECUTE FUNCTION tombot.reject_catalog_event()`)
		_, err = fixture.service.Update(ctx, fixture.a, bot.ID, UpdateRequest{bot.Revision, spec("不应提交")})
		if err == nil {
			t.Fatal("事件失败被忽略")
		}
		loaded, err := fixture.service.Get(ctx, fixture.a, bot.ID)
		if err != nil || loaded.Name != bot.Name || loaded.Revision != bot.Revision {
			t.Fatal("业务与事件未共同回滚", err)
		}
		_, _, err = fixture.service.CreateGroup(ctx, fixture.a, CreateGroupRequest{ids.New(), GroupSpec{Name: "不应存在", MemberIDs: []string{bot.ID}}})
		if err == nil {
			t.Fatal("群组事件失败被忽略")
		}
		groups, err := fixture.service.ListGroups(ctx, fixture.a, "", 10, false)
		if err != nil || len(groups) != 0 {
			t.Fatal("群组部分提交", err)
		}
	})
	t.Run("会话撤销与数据库归属约束", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		first := fixture.create(t, fixture.a, "本人")
		foreign := fixture.create(t, fixture.b, "他人")
		err := fixture.tenant.Tenant(ctx, fixture.a.UserID, func(transaction *sql.Tx) error {
			_, err := transaction.ExecContext(ctx, `INSERT INTO tombot.conversations(user_id,id,kind,bot_id,title) VALUES($1,$2,'direct',$3,'越权')`, fixture.a.UserID, ids.New(), foreign.ID)
			return err
		})
		var state *pgconn.PgError
		if !errors.As(err, &state) || state.Code != "23503" {
			t.Fatal("数据库未拒绝跨用户会话", err)
		}
		err = fixture.tenant.Tenant(ctx, fixture.a.UserID, func(transaction *sql.Tx) error {
			var count int
			if err := transaction.QueryRowContext(ctx, `SELECT count(*) FROM tombot.conversations`).Scan(&count); err != nil {
				return err
			}
			if count != 1 {
				return errors.New("会话 RLS 泄漏其他用户")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		spoofed := fixture.a
		spoofed.Scope = "admin"
		_, err = fixture.service.Get(ctx, spoofed, first.ID)
		expect(t, err, identity.ErrInvalidSession)
		spoofed = fixture.a
		spoofed.AuthVersion++
		_, err = fixture.service.Get(ctx, spoofed, first.ID)
		expect(t, err, identity.ErrInvalidSession)
		execute(t, fixture.pool, `UPDATE tombot.user_sessions SET revoked_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, fixture.a.UserID, fixture.a.SessionID)
		_, err = fixture.service.Get(ctx, fixture.a, first.ID)
		expect(t, err, identity.ErrInvalidSession)
		_, _, err = fixture.service.Create(ctx, fixture.a, CreateRequest{ids.New(), spec("撤销后")})
		expect(t, err, identity.ErrInvalidSession)
	})
	t.Run("业务事务复核账户设备与过期", func(t *testing.T) {
		for _, statement := range []string{
			`UPDATE tombot.users SET status='disabled' WHERE id=$1`,
			`UPDATE tombot.devices SET revoked_at=clock_timestamp() WHERE user_id=$1`,
			`UPDATE tombot.user_sessions SET created_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE user_id=$1`,
		} {
			fixture := newFixture(t, cluster)
			bot := fixture.create(t, fixture.a, "复核身份")
			execute(t, fixture.pool, statement, fixture.a.UserID)
			_, err := fixture.service.Get(ctx, fixture.a, bot.ID)
			expect(t, err, identity.ErrInvalidSession)
			_, err = fixture.service.Update(ctx, fixture.a, bot.ID, UpdateRequest{bot.Revision, bot.Spec})
			expect(t, err, identity.ErrInvalidSession)
			if _, err = fixture.service.Snapshot(ctx, fixture.b, 1); err != nil {
				t.Fatal("身份失效影响其他用户", err)
			}
		}
	})
	t.Run("数据库幂等记录与头像所属关系", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		first := fixture.create(t, fixture.a, "约束本人")
		foreign := fixture.create(t, fixture.b, "约束他人")
		for _, target := range []string{"bots", "groups"} {
			for _, values := range []struct {
				key    any
				digest any
			}{
				{nil, bytes.Repeat([]byte{1}, 32)}, {ids.New(), nil}, {ids.New(), []byte{1}},
			} {
				err := fixture.tenant.Tenant(ctx, fixture.a.UserID, func(transaction *sql.Tx) error {
					query := `INSERT INTO tombot.` + target + `(user_id,id,name,client_request_id,creation_digest) VALUES($1,$2,$3,$4,$5)`
					arguments := []any{fixture.a.UserID, ids.New(), ids.New(), values.key, values.digest}
					if target == "bots" {
						query = `INSERT INTO tombot.bots(user_id,id,name,client_request_id,creation_digest,desktop_id) VALUES($1,$2,$3,$4,$5,$6)`
						arguments = append(arguments, ids.New())
					}
					_, err := transaction.ExecContext(ctx, query, arguments...)
					return err
				})
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || failure.Code != "23514" {
					t.Fatal("无效幂等记录未被数据库拒绝", target, err)
				}
			}
		}
		err := fixture.tenant.Tenant(ctx, fixture.a.UserID, func(transaction *sql.Tx) error {
			_, err := transaction.ExecContext(ctx, `INSERT INTO tombot.bot_avatars(user_id,bot_id,content,sha256) VALUES($1,$2,$3,$4)`, fixture.a.UserID, foreign.ID, []byte{1}, secure.Hash("image"))
			return err
		})
		var failure *pgconn.PgError
		if !errors.As(err, &failure) || failure.Code != "23503" {
			t.Fatal("跨用户头像未被数据库拒绝", err)
		}
		err = fixture.tenant.Tenant(ctx, fixture.a.UserID, func(transaction *sql.Tx) error {
			_, err := transaction.ExecContext(ctx, `INSERT INTO tombot.conversations(user_id,id,kind,bot_id,peer_bot_id,title) VALUES($1,$2,'bot_dm',$3,$3,'自己对话')`, fixture.a.UserID, ids.New(), first.ID)
			return err
		})
		if !errors.As(err, &failure) || failure.Code != "23514" {
			t.Fatal("无效私信参与方未被数据库拒绝", err)
		}
	})
}

func TestCatalogValidation(t *testing.T) {
	service := &Service{tools: map[string]bool{"file_read": true}}
	for _, change := range []func(*Spec){
		func(s *Spec) { s.Name = " " }, func(s *Spec) { s.Name = "名称\x00" }, func(s *Spec) { s.Name = strings.Repeat("a", 121) },
		func(s *Spec) { s.SystemPrompt = "" }, func(s *Spec) { s.ToolPermissions = []string{"unknown"} }, func(s *Spec) { s.ToolPermissions = []string{"file_read", "file_read"} },
		func(s *Spec) { s.ToolPermissions = nil }, func(s *Spec) { s.AvatarRef = "https://example.test/a.png" }, func(s *Spec) { s.ApprovalMode = "bypass" }, func(s *Spec) { s.AccentHue = 360 },
	} {
		value := spec("测试")
		value.ToolPermissions = []string{"file_read"}
		change(&value)
		_, err := service.normalize(value)
		expect(t, err, ErrInvalidInput)
	}
	for _, members := range [][]string{nil, {ids.New(), "invalid"}, func() []string { id := ids.New(); return []string{id, id} }()} {
		_, err := normalizeGroup(GroupSpec{Name: "群组", MemberIDs: members})
		expect(t, err, ErrInvalidInput)
	}
}
