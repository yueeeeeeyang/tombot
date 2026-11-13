package userapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/authapi"
	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/execution"
	"io.yueyang/tom/internal/identity"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
	"io.yueyang/tom/internal/userevents"
)

func execute(t *testing.T, pool *sql.DB, query string, arguments ...any) {
	t.Helper()
	if _, err := pool.Exec(query, arguments...); err != nil {
		t.Fatal("准备用户 API 联合测试失败", err)
	}
}
func randomValue(t *testing.T) string {
	t.Helper()
	value, err := secure.Token()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

type reply struct {
	status int
	body   []byte
	header http.Header
}

func decode[T any](t *testing.T, response reply, status int) T {
	t.Helper()
	if response.status != status {
		t.Fatalf("预期 HTTP %d，实际 %d", status, response.status)
	}
	var result T
	if err := json.Unmarshal(response.body, &result); err != nil {
		t.Fatal("响应不是预期的 JSON", err)
	}
	return result
}

type botReply struct {
	Bot       bots.Bot `json:"bot"`
	IsCreated bool     `json:"is_created"`
}
type groupReply struct {
	Group     bots.Group `json:"group"`
	IsCreated bool       `json:"is_created"`
}

// HTTP 入队测试不执行 Worker；意外解析模型必须失败，不能伪造可调用配置。
type unavailableResolver struct{}

func (unavailableResolver) Resolve(context.Context, string, string) (modelregistry.Selection, error) {
	return modelregistry.Selection{}, modelregistry.ErrUnavailable
}

func TestPostgreSQLAndHTTPSUserResources(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	open := func(role postgres.Role) *postgres.Database {
		t.Helper()
		login := cluster.Login(t, string(role))
		database, err := postgres.Open(ctx, postgres.Options{DSN: cluster.DSN(name, login), Role: role, MaxConnections: 4, ConnectTimeout: 5 * time.Second, StatementTimeout: 5 * time.Second, LockTimeout: time.Second, TransactionTimeout: 10 * time.Second, ConnectionLifetime: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := database.Close(); err != nil {
				t.Error(err)
			}
		})
		return database
	}
	key, err := secure.Random(32)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	identityService, err := identity.New(open(postgres.IdentityRole), identity.Options{AccessLifetime: 5 * time.Minute, SessionLifetime: time.Hour, VerificationLifetime: 5 * time.Minute, RateWindow: time.Minute, AccountAttempts: 50, SourceAttempts: 100, PasswordWorkers: 2, DeviceLimit: 10, EncryptionKey: key})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	csrf, err := secure.Random(32)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(csrf)
	gate, err := authapi.New(identityService, authapi.Options{AdminOrigin: "https://admin.example.test", CSRFKey: csrf, RequestTimeout: 10 * time.Second}, logger)
	if err != nil {
		t.Fatal(err)
	}
	tenant := open(postgres.TenantRole)
	catalog, err := bots.New(tenant, bots.Options{MaxBots: 20, MaxGroups: 10, ToolNames: []string{"file_read", "send_user_message", "send_bot_message"}})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := execution.New(tenant, open(postgres.DispatchRole), unavailableResolver{}, execution.Options{LeaseLifetime: 10 * time.Second, RecoveryDelay: time.Second, MaxAttempts: 3, MaxOperations: 8, ContextMessages: 20, ContextBytes: 128 << 10, Images: execution.ImageOptions{MaxUserBytes: 64 << 20, MaxUserImages: 64, ContextImages: 2, Retention: 7 * 24 * time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	userEvents, err := userevents.New(tenant)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(catalog, executor, userEvents, gate, logger)
	if err != nil {
		t.Fatal(err)
	}
	// 联合测试把一次订阅压缩为"读满一轮即收尾"，避免等待真实的分钟级窗口。
	handler.eventTiming = eventTiming{pollInterval: 10 * time.Millisecond, heartbeat: 20 * time.Millisecond, drainMargin: 500 * time.Millisecond, minimumWindow: time.Hour}
	mux := http.NewServeMux()
	mux.Handle("/api/v2/auth/", gate)
	mux.Handle("/", handler)
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 10 * time.Second
	call := func(method, path string, body any, token identity.Credential, headers map[string]string) reply {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest(method, server.URL+path, bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+string(token))
		}
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("用户 HTTPS 请求失败")
		}
		content, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatal("用户 HTTP 响应读取失败")
		}
		if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Request-ID") == "" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("响应缺少认证门的缓存或请求标识")
		}
		return reply{response.StatusCode, content, response.Header}
	}
	node, imageID := ids.New(), ids.New()
	execute(t, pool, `INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, imageID, "example.test/computer@sha256:"+secure.Hash(imageID))
	execute(t, pool, `INSERT INTO tombot.computer_nodes(id,name,status,capacity,runtime_id,control_sequence,last_seen_at,control_expires_at,verified_image_id,capability_expires_at) VALUES($1,$1,'ready',10,$1,1,clock_timestamp(),clock_timestamp()+interval '5 minutes',$2,clock_timestamp()+interval '15 minutes')`, node, imageID)
	sessions := make([]identity.Session, 2)
	for index := range sessions {
		user, computer, activation, password := ids.New(), ids.New(), randomValue(t), randomValue(t)
		username := strings.ToLower(user)
		execute(t, pool, `INSERT INTO tombot.users(id,username,display_name,status) VALUES($1,$2,'业务用户','provisioning')`, user, username)
		execute(t, pool, `INSERT INTO tombot.computers(user_id,id,node_id,image_id,status,container_id,volume_ref,cpu_millis,memory_bytes,disk_bytes,process_limit) VALUES($1,$2,$3,$4,'ready',$2,$2,1000,1073741824,1073741824,128)`, user, computer, node, imageID)
		execute(t, pool, `UPDATE tombot.users SET status='pending_activation' WHERE id=$1`, user)
		execute(t, pool, `INSERT INTO tombot.user_limits(user_id,concurrent_model_calls,concurrent_desktops,queued_runs,monthly_token_limit) VALUES($1,2,2,10,100000)`, user)
		execute(t, pool, `INSERT INTO tombot.activation_tokens(id,user_id,token_digest,expires_at) VALUES($1,$2,decode($3,'hex'),clock_timestamp()+interval '1 hour')`, ids.New(), user, secure.Hash(activation))
		if response := call("POST", "/api/v2/auth/activate", map[string]string{"activation_token": activation, "password": password}, "", nil); response.status != 204 {
			t.Fatal("账户激活失败", response.status)
		}
		sessions[index] = decode[identity.Session](t, call("POST", "/api/v2/auth/login", map[string]string{"username": username, "password": password, "device_name": "业务接口测试", "platform": "linux"}, "", nil), 200)
		if sessions[index].Principal.UserID != user {
			t.Fatal("用户身份错误")
		}
	}
	first, second := sessions[0].AccessToken, sessions[1].AccessToken
	spec := bots.Spec{Name: "研究员", Description: "用户自己的自定义资料", SystemPrompt: "You are a research bot. Use verified evidence.", ToolPermissions: []string{"file_read", "send_user_message"}, ApprovalMode: "standard", MessagingPolicy: bots.MessagingPolicy{AcceptDM: true, CanInitiate: true, AcceptGroupInvite: true}, AccentHue: 125, AvatarRef: "client-asset-1"}
	request := bots.CreateRequest{ClientRequestID: ids.New(), Spec: spec}
	created := decode[botReply](t, call("POST", "/api/v2/bots", request, first, nil), 201)
	if !created.IsCreated || created.Bot.DesktopID == "" || created.Bot.ConversationID == "" || created.Bot.SystemPrompt != spec.SystemPrompt {
		t.Fatal("机器人创建响应错误")
	}
	replayed := decode[botReply](t, call("POST", "/api/v2/bots", request, first, nil), 200)
	if replayed.IsCreated || replayed.Bot.ID != created.Bot.ID || replayed.Bot.DesktopID != created.Bot.DesktopID {
		t.Fatal("创建请求被重复执行")
	}
	for _, field := range []string{"desktop_id", "linux_uid", "shared_gid", "display_number"} {
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		var forged map[string]json.RawMessage
		if err = json.Unmarshal(encoded, &forged); err != nil {
			t.Fatal(err)
		}
		forged[field] = json.RawMessage(`"client-controlled"`)
		if reply := call("POST", "/api/v2/bots", forged, first, nil); reply.status != 400 {
			t.Fatal("客户端可以指定桌面运行身份", field, reply.status)
		}
	}
	foreign := decode[botReply](t, call("POST", "/api/v2/bots", request, second, nil), 201)
	if foreign.Bot.ID == created.Bot.ID {
		t.Fatal("跨账户复用了同一资源")
	}
	conversationPath := "/api/v2/conversations/" + created.Bot.ConversationID
	messagePath := conversationPath + "/messages"
	messageRequest := execution.SendRequest{ClientRequestID: ids.New(), Content: "请核对这份资料。"}
	type submissionReply struct {
		Submission execution.Submission `json:"submission"`
	}
	type runReply struct {
		Run execution.Run `json:"run"`
	}
	type messagesReply struct {
		Messages []execution.Message `json:"messages"`
	}
	accepted := call("POST", messagePath, messageRequest, first, nil)
	submission := decode[submissionReply](t, accepted, 202).Submission
	runPath := "/api/v2/runs/" + submission.RunID
	if !submission.IsCreated || accepted.header.Get("Location") != runPath {
		t.Fatal("消息未返回持久运行地址")
	}
	if replay := decode[submissionReply](t, call("POST", messagePath, messageRequest, first, nil), 200).Submission; replay.IsCreated || replay.RunID != submission.RunID || replay.MessageID != submission.MessageID {
		t.Fatal("消息重试重复入队")
	}
	if run := decode[runReply](t, call("GET", runPath, nil, first, nil), 200).Run; run.Status != "queued" || run.ConversationID != created.Bot.ConversationID {
		t.Fatal("消息受理被误报为执行成功")
	}
	for _, test := range []struct {
		method, path string
		body         any
	}{
		{"GET", messagePath, nil}, {"POST", messagePath, messageRequest},
		{"POST", conversationPath + "/read", map[string]int{"sequence": 1}},
		{"GET", runPath, nil}, {"POST", runPath + "/cancel", map[string]any{}},
	} {
		if response := call(test.method, test.path, test.body, second, nil); response.status != 404 {
			t.Fatal("跨用户消息或运行未隐藏", test.path, response.status)
		}
	}
	messages := decode[messagesReply](t, call("GET", messagePath+"?limit=1", nil, first, nil), 200).Messages
	if len(messages) != 1 || messages[0].ID != submission.MessageID || messages[0].Blocks[0].Text != messageRequest.Content {
		t.Fatal("消息正文未持久返回")
	}
	if page := decode[messagesReply](t, call("GET", messagePath+"?before=1", nil, first, nil), 200).Messages; len(page) != 0 {
		t.Fatal("消息游标重复返回记录")
	}
	if response := call("POST", conversationPath+"/read", map[string]int{"sequence": 1}, first, nil); response.status != 204 {
		t.Fatal("已读提交失败", response.status)
	}
	if response := call("POST", messagePath, execution.SendRequest{ClientRequestID: ids.New(), Content: "下一回合"}, first, nil); response.status != 409 {
		t.Fatal("未限制活动会话", response.status)
	}
	for range 2 {
		if run := decode[runReply](t, call("POST", runPath+"/cancel", map[string]any{}, first, nil), 200).Run; run.Status != "canceled" {
			t.Fatal("取消排队任务不幂等")
		}
	}
	next := decode[submissionReply](t, call("POST", messagePath, execution.SendRequest{ClientRequestID: ids.New(), Content: "新的回合"}, first, nil), 202).Submission
	if messages := decode[messagesReply](t, call("GET", messagePath+"?before=3&limit=1", nil, first, nil), 200).Messages; len(messages) != 1 || messages[0].Sequence != 2 || messages[0].ID != next.MessageID {
		t.Fatal("消息分页顺序错误")
	}
	decode[runReply](t, call("POST", "/api/v2/runs/"+next.RunID+"/cancel", map[string]any{}, first, nil), 200)
	botPath := "/api/v2/bots/" + created.Bot.ID
	for _, path := range []string{botPath, "/api/v2/conversations/" + created.Bot.ConversationID, botPath + "/avatar"} {
		if response := call("GET", path, nil, second, nil); response.status != 404 {
			t.Fatal("跨用户资源未隐藏", path, response.status)
		}
	}
	var page struct {
		Bots []bots.Bot `json:"bots"`
	}
	page = decode[struct {
		Bots []bots.Bot `json:"bots"`
	}](t, call("GET", "/api/v2/bots?limit=1", nil, first, nil), 200)
	if len(page.Bots) != 1 || page.Bots[0].ID != created.Bot.ID {
		t.Fatal("机器人列表越权或丢失")
	}
	page = decode[struct {
		Bots []bots.Bot `json:"bots"`
	}](t, call("GET", "/api/v2/bots?before="+created.Bot.ID+"&limit=1", nil, first, nil), 200)
	if len(page.Bots) != 0 {
		t.Fatal("游标重复返回记录")
	}
	updatedSpec := spec
	updatedSpec.Name = "新研究员"
	updated := decode[botReply](t, call("PUT", botPath, bots.UpdateRequest{ExpectedRevision: 1, Spec: updatedSpec}, first, nil), 200)
	if updated.Bot.Revision != 2 {
		t.Fatal("修订未更新")
	}
	if response := call("PUT", botPath, bots.UpdateRequest{ExpectedRevision: 1, Spec: spec}, first, nil); response.status != 409 {
		t.Fatal("旧修订覆盖", response.status)
	}
	var picture bytes.Buffer
	if err = png.Encode(&picture, image.NewNRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		t.Fatal(err)
	}
	updated = decode[botReply](t, call("PUT", botPath+"/avatar", map[string]any{"expected_revision": updated.Bot.Revision, "content_base64": base64.StdEncoding.EncodeToString(picture.Bytes())}, first, nil), 200)
	avatar := call("GET", botPath+"/avatar", nil, first, nil)
	if avatar.status != 200 || !bytes.Equal(avatar.body, picture.Bytes()) || avatar.header.Get("Content-Type") != "image/png" || !updated.Bot.HasCustomAvatar {
		t.Fatal("头像返回错误")
	}
	if response := call("GET", botPath+"/avatar", nil, second, nil); response.status != 404 {
		t.Fatal("跨用户头像泄漏")
	}
	updated = decode[botReply](t, call("DELETE", botPath+"/avatar", map[string]int64{"expected_revision": updated.Bot.Revision}, first, nil), 200)
	if updated.Bot.HasCustomAvatar {
		t.Fatal("删除头像未生效")
	}
	groupRequest := bots.CreateGroupRequest{ClientRequestID: ids.New(), GroupSpec: bots.GroupSpec{Name: "资料工作组", MemberIDs: []string{created.Bot.ID}}}
	group := decode[groupReply](t, call("POST", "/api/v2/groups", groupRequest, first, nil), 201)
	groupPath := "/api/v2/groups/" + group.Group.ID
	kindFailure := decode[struct {
		Code string `json:"code"`
	}](t, call("POST", "/api/v2/conversations/"+group.Group.ConversationID+"/messages", execution.SendRequest{ClientRequestID: ids.New(), Content: "群聊任务"}, first, nil), 409)
	if kindFailure.Code != "CONVERSATION_KIND_UNAVAILABLE" {
		t.Fatal("尚未接入的群编排未明确拒绝")
	}
	if response := call("GET", groupPath, nil, second, nil); response.status != 404 {
		t.Fatal("跨用户群组泄漏")
	}
	if result := decode[groupReply](t, call("POST", "/api/v2/groups", groupRequest, first, nil), 200); result.IsCreated || result.Group.ID != group.Group.ID {
		t.Fatal("群组重试不幂等")
	}
	groupSpec := group.Group.GroupSpec
	groupSpec.Name = "更新的工作组"
	group = decode[groupReply](t, call("PUT", groupPath, bots.UpdateGroupRequest{ExpectedRevision: group.Group.Revision, GroupSpec: groupSpec}, first, nil), 200)
	groupSpec.MemberIDs = []string{foreign.Bot.ID}
	if response := call("PUT", groupPath, bots.UpdateGroupRequest{ExpectedRevision: group.Group.Revision, GroupSpec: groupSpec}, first, nil); response.status != 404 {
		t.Fatal("允许跨用户成员", response.status)
	}
	groups := decode[struct {
		Groups []bots.Group `json:"groups"`
	}](t, call("GET", "/api/v2/groups", nil, first, nil), 200)
	if len(groups.Groups) != 1 || groups.Groups[0].Name != "更新的工作组" {
		t.Fatal("群组目录不一致")
	}
	conversation := decode[struct {
		Conversation bots.Conversation `json:"conversation"`
	}](t, call("GET", "/api/v2/conversations/"+group.Group.ConversationID, nil, first, nil), 200)
	if conversation.Conversation.Title != "更新的工作组" {
		t.Fatal("群会话未同步")
	}
	conversations := decode[struct {
		Conversations []bots.Conversation `json:"conversations"`
	}](t, call("GET", "/api/v2/conversations", nil, first, nil), 200)
	if len(conversations.Conversations) != 2 {
		t.Fatal("会话目录数量错误")
	}
	snapshot := decode[struct {
		Snapshot bots.Snapshot `json:"snapshot"`
	}](t, call("GET", "/api/v2/workspace?limit=10", nil, first, nil), 200).Snapshot
	if len(snapshot.Bots) != 1 || len(snapshot.Groups) != 1 || snapshot.EventSequence < 1 {
		t.Fatal("工作空间未使用用户目录")
	}
	workspace := decode[struct {
		Workspace bots.Workspace `json:"workspace"`
	}](t, call("PUT", "/api/v2/workspace", bots.WorkspaceRequest{ExpectedRevision: snapshot.Workspace.Revision, PrimaryBotID: &created.Bot.ID}, first, nil), 200).Workspace
	if workspace.ActiveBotID != nil || workspace.PrimaryBotID == nil {
		t.Fatal("工作空间选择语义错误")
	}
	verifyEventStream(t, server.URL, client, first, second, created.Bot.ID, snapshot.EventSequence)
	verifyExpiredEventCursor(t, server.URL, client, first, open(postgres.ControlRole), sessions[0].Principal.UserID)
	group = decode[groupReply](t, call("POST", groupPath+"/state", map[string]any{"expected_revision": group.Group.Revision, "is_archived": true}, first, nil), 200)
	if !group.Group.IsArchived {
		t.Fatal("群组归档未生效")
	}
	group = decode[groupReply](t, call("POST", groupPath+"/state", map[string]any{"expected_revision": group.Group.Revision, "is_archived": false}, first, nil), 200)
	updated = decode[botReply](t, call("POST", botPath+"/state", map[string]any{"expected_revision": updated.Bot.Revision, "is_archived": true}, first, nil), 200)
	if !updated.Bot.IsArchived {
		t.Fatal("机器人归档未生效")
	}
	updated = decode[botReply](t, call("POST", botPath+"/state", map[string]any{"expected_revision": updated.Bot.Revision, "is_archived": false}, first, nil), 200)
	for _, test := range []struct {
		method, path string
		body         any
		status       int
	}{
		{"GET", messagePath + "?before=0", nil, 400}, {"GET", messagePath + "?before=-1", nil, 400},
		{"GET", messagePath + "?before=01", nil, 400}, {"GET", messagePath + "?before=9223372036854775808", nil, 400},
		{"GET", messagePath + "?limit=101", nil, 400}, {"GET", messagePath + "?limit=1&limit=2", nil, 400},
		{"GET", messagePath + "?user_id=" + sessions[1].Principal.UserID, nil, 400},
		{"POST", messagePath, map[string]string{"client_request_id": ids.New(), "content": "消息", "model_id": "client-model"}, 400},
		{"POST", messagePath, map[string]string{"client_request_id": ids.New(), "content": "消息", "user_id": sessions[1].Principal.UserID}, 400},
		{"POST", messagePath, execution.SendRequest{ClientRequestID: ids.New(), Content: strings.Repeat("a", (32<<10)+1)}, 400},
		{"POST", conversationPath + "/read", map[string]any{}, 400},
		{"POST", conversationPath + "/read", map[string]int{"sequence": 3}, 400},
		{"POST", runPath + "/cancel", map[string]string{"lease_owner": ids.New()}, 400},
		{"POST", runPath + "/cancel?user_id=x", map[string]any{}, 400},
		{"POST", "/api/v2/jobs/claim", map[string]any{}, 404},
		{"GET", "/api/v2/bots?user_id=" + sessions[1].Principal.UserID, nil, 400},
		{"GET", "/api/v2/bots?limit=101", nil, 400}, {"GET", "/api/v2/bots?limit=1&limit=2", nil, 400}, {"GET", "/api/v2/bots?archived=1", nil, 400},
		{"GET", "/api/v2/bots?before=invalid", nil, 400}, {"GET", "/api/v2/workspace?before=" + created.Bot.ID, nil, 400},
		{"POST", "/api/v2/bots", json.RawMessage(`{"user_id":"override"}`), 400},
		{"POST", "/api/v2/bots", json.RawMessage(`{"model_id":"client-model"}`), 400},
		{"PUT", botPath, json.RawMessage(`{"id":"override"}`), 400},
		{"POST", botPath + "/state", map[string]int{"expected_revision": 1}, 400},
		{"PUT", botPath + "/avatar", map[string]any{"expected_revision": updated.Bot.Revision, "content_base64": "not-base64"}, 400},
		{"PUT", botPath + "/avatar", map[string]any{"expected_revision": updated.Bot.Revision, "content_base64": strings.Repeat("a", 101<<10)}, 400},
		{"POST", "/api/v2/bots?limit=1", request, 400}, {"GET", botPath + "?user_id=x", nil, 400},
		{"GET", "/api/v2/models", nil, 404}, {"GET", "/api/v1/bots", nil, 404}, {"DELETE", botPath, nil, 405},
	} {
		if response := call(test.method, test.path, test.body, first, nil); response.status != test.status {
			t.Fatal("边界响应错误", test.method, test.path, response.status, test.status)
		}
	}
	if response := call("GET", "/api/v2/bots", nil, "", nil); response.status != 401 {
		t.Fatal("未登录读取目录")
	}
	if response := call("GET", "/api/v2/bots", nil, "", map[string]string{"Cookie": "__Host-tombot-access=" + string(first)}); response.status != 401 {
		t.Fatal("管理 Cookie 被用于用户内容")
	}
	if response := call("POST", "/api/v2/bots", request, first, map[string]string{"Origin": "https://untrusted.example.test"}); response.status != 403 {
		t.Fatal("跨来源未拒绝")
	}
	execute(t, pool, `CREATE FUNCTION tombot.reject_api_event() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN RAISE EXCEPTION 'PRIVATE_DATABASE_DIAGNOSTIC'; END $body$`)
	execute(t, pool, `CREATE TRIGGER reject_api_event BEFORE INSERT ON tombot.user_events FOR EACH ROW EXECUTE FUNCTION tombot.reject_api_event()`)
	failure := call("PUT", botPath, bots.UpdateRequest{ExpectedRevision: updated.Bot.Revision, Spec: spec}, first, nil)
	if failure.status != 503 || bytes.Contains(failure.body, []byte("PRIVATE_DATABASE_DIAGNOSTIC")) || strings.Contains(logs.String(), "PRIVATE_DATABASE_DIAGNOSTIC") || strings.Contains(logs.String(), spec.SystemPrompt) {
		t.Fatal("底层错误或业务正文泄漏")
	}
	if response := call("POST", "/api/v2/auth/logout", map[string]any{}, first, nil); response.status != 204 {
		t.Fatal("退出失败", response.status)
	}
	if response := call("GET", "/api/v2/workspace", nil, first, nil); response.status != 401 {
		t.Fatal("撤销后仍读取内容")
	}
	if response := call("GET", messagePath, nil, first, nil); response.status != 401 {
		t.Fatal("撤销后仍读取消息")
	}
	if response := call("GET", "/api/v2/workspace", nil, second, nil); response.status != 200 {
		t.Fatal("撤销影响其他用户")
	}
	execute(t, pool, `UPDATE tombot.users SET status='disabled' WHERE id=$1`, sessions[1].Principal.UserID)
	if response := call("GET", "/api/v2/workspace", nil, second, nil); response.status != 401 {
		t.Fatal("禁用后仍读取内容")
	}
}

// eventFrame 是测试从 SSE 正文中解析出的一帧；只校验协议必需字段。
type eventFrame struct {
	kind string
	id   int64
	data string
}

// parseEventStream 按空行切分帧，忽略重连提示与心跳注释。
func parseEventStream(t *testing.T, content string) []eventFrame {
	t.Helper()
	frames := make([]eventFrame, 0, 8)
	for _, block := range strings.Split(content, "\n\n") {
		if strings.TrimSpace(block) == "" || strings.HasPrefix(block, ":") || strings.HasPrefix(block, "retry:") {
			continue
		}
		var frame eventFrame
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				frame.kind = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				value, err := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
				if err != nil {
					t.Fatal("事件帧序号不是整数", line)
				}
				frame.id = value
			case strings.HasPrefix(line, "data: "):
				frame.data = strings.TrimPrefix(line, "data: ")
			}
		}
		// 错误帧不推进游标，因此没有有效序号；普通事件必须带序号。
		if frame.kind == "" || frame.data == "" || frame.kind != "error" && frame.id == 0 {
			t.Fatal("事件帧缺少必要字段", block)
		}
		frames = append(frames, frame)
	}
	return frames
}

// readEventStream 发起一次订阅并读到服务端收尾；调用方必须先把 timing 收窄到立即收尾。
func readEventStream(t *testing.T, baseURL string, client *http.Client, token identity.Credential, query string, headers map[string]string) []eventFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/api/v2/events"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+string(token))
	request.Header.Set("Accept", "text/event-stream")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("事件流请求失败", err)
	}
	content, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	closeErr := response.Body.Close()
	if response.StatusCode != 200 || readErr != nil || closeErr != nil {
		t.Fatal("事件流未正常返回", response.StatusCode, readErr, closeErr)
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatal("事件流内容类型错误", response.Header.Get("Content-Type"))
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("事件流缺少缓存禁止头")
	}
	return parseEventStream(t, string(content))
}

// verifyEventStream 覆盖首订阅、按序号续订、Last-Event-ID 续订和跨用户隔离。
func verifyEventStream(t *testing.T, baseURL string, client *http.Client, first, second identity.Credential, botID string, watermark int64) {
	t.Helper()
	all := readEventStream(t, baseURL, client, first, "?after=0", nil)
	if len(all) == 0 {
		t.Fatal("事件流没有返回已提交事件")
	}
	created := false
	// 快照水位是读取时刻的提交位置；此后的新事件天然更大，只校验单调递增与合法性。
	var previous int64
	for _, frame := range all {
		if frame.kind == "bot.created" && strings.Contains(frame.data, botID) {
			created = true
		}
		if frame.id < 1 || frame.id <= previous {
			t.Fatal("事件序号不是严格递增", frame.id, previous)
		}
		previous = frame.id
	}
	if !created {
		t.Fatal("事件流未包含机器人创建")
	}
	last := all[len(all)-1].id
	if len(readEventStream(t, baseURL, client, first, "?after="+strconv.FormatInt(last, 10), nil)) != 0 {
		t.Fatal("按序号续订重复返回事件")
	}
	if len(readEventStream(t, baseURL, client, first, "", map[string]string{"Last-Event-ID": strconv.FormatInt(last, 10)})) != 0 {
		t.Fatal("Last-Event-ID 续订重复返回事件")
	}
	for _, frame := range readEventStream(t, baseURL, client, second, "?after=0", nil) {
		if strings.Contains(frame.data, botID) {
			t.Fatal("跨用户事件泄漏", frame.data)
		}
	}
}

// verifyExpiredEventCursor 清理到只剩最新事件后，旧游标必须收到明确的重新同步指令，
// 而不是静默返回空批次——否则客户端会永久错过已删除的变更。
func verifyExpiredEventCursor(t *testing.T, baseURL string, client *http.Client, token identity.Credential, control *postgres.Database, userID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := control.Platform(ctx, func(transaction *sql.Tx) error {
		_, err := postgres.PruneUserEvents(ctx, transaction, userID, 1, 100)
		return err
	}); err != nil {
		t.Fatal("清理用户事件失败", err)
	}
	frames := readEventStream(t, baseURL, client, token, "?after=0", nil)
	if len(frames) != 1 || frames[0].kind != "error" || !strings.Contains(frames[0].data, "EVENT_CURSOR_EXPIRED") {
		t.Fatal("过期游标未要求重新同步", frames)
	}
}
