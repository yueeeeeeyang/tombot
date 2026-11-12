package authapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/identity"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func integrationExecute(t *testing.T, pool *sql.DB, query string, arguments ...any) {
	t.Helper()
	if _, err := pool.Exec(query, arguments...); err != nil {
		t.Fatal("准备认证联合测试失败", err)
	}
}

func TestPostgreSQLAndHTTPSIdentityFlow(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	role := cluster.Login(t, string(postgres.IdentityRole))
	database, err := postgres.Open(ctx, postgres.Options{DSN: cluster.DSN(name, role), Role: postgres.IdentityRole, MaxConnections: 4,
		ConnectTimeout: 5 * time.Second, StatementTimeout: 5 * time.Second, LockTimeout: time.Second, TransactionTimeout: 10 * time.Second, ConnectionLifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	key, err := secure.Random(32)
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.New(database, identity.Options{AccessLifetime: 5 * time.Minute, SessionLifetime: time.Hour, VerificationLifetime: 5 * time.Minute,
		RateWindow: time.Minute, AccountAttempts: 50, SourceAttempts: 100, PasswordWorkers: 2, DeviceLimit: 10, EncryptionKey: key})
	if err != nil {
		t.Fatal(err)
	}
	csrfKey, err := secure.Random(32)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(service, Options{AdminOrigin: "https://admin.example.test", CSRFKey: csrfKey, RequestTimeout: 10 * time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	client := server.Client()
	client.Timeout = 10 * time.Second
	call := func(method, path string, body any, token identity.Credential, target any) int {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal("认证请求编码失败")
		}
		r, err := http.NewRequest(method, server.URL+path, bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+string(token))
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal("认证 HTTPS 联合请求失败")
		}
		if target != nil && response.StatusCode == 200 {
			if err = json.NewDecoder(response.Body).Decode(target); err != nil {
				t.Fatal("认证响应格式错误")
			}
		} else if _, err = io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal("读取认证响应失败")
		}
		if err = response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return response.StatusCode
	}
	node, image := ids.New(), ids.New()
	integrationExecute(t, pool, `INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, image, "registry.example.test/computer@sha256:"+secure.Hash(randomValue(t)))
	integrationExecute(t, pool, `INSERT INTO tombot.computer_nodes(id,name,status,capacity,runtime_id,control_sequence,last_seen_at,control_expires_at,verified_image_id,capability_expires_at) VALUES($1,$1,'ready',10,$1,1,clock_timestamp(),clock_timestamp()+interval '5 minutes',$2,clock_timestamp()+interval '15 minutes')`, node, image)
	sessions := make([]identity.Session, 2)
	for index := range 2 {
		user, computer, activation, password := ids.New(), ids.New(), randomValue(t), randomValue(t)
		username := strings.ToLower(user)
		integrationExecute(t, pool, `INSERT INTO tombot.users(id,username,display_name,role,status) VALUES($1,$2,'联合测试用户','user','provisioning')`, user, username)
		integrationExecute(t, pool, `INSERT INTO tombot.computers(user_id,id,node_id,image_id,status,container_id,volume_ref,cpu_millis,memory_bytes,disk_bytes,process_limit)
			VALUES($1,$2,$3,$4,'ready',$2,$2,1000,1073741824,1073741824,128)`, user, computer, node, image)
		integrationExecute(t, pool, `UPDATE tombot.users SET status='pending_activation' WHERE id=$1`, user)
		integrationExecute(t, pool, `INSERT INTO tombot.activation_tokens(id,user_id,token_digest,expires_at) VALUES($1,$2,decode($3,'hex'),clock_timestamp()+interval '1 hour')`, ids.New(), user, secure.Hash(activation))
		if status := call("POST", "/api/v2/auth/activate", map[string]string{"activation_token": activation, "password": password}, "", nil); status != 204 {
			t.Fatal("HTTPS 激活失败", status)
		}
		if status := call("POST", "/api/v2/auth/login", map[string]string{"username": username, "password": password, "device_name": "联合测试客户端", "platform": "linux"}, "", &sessions[index]); status != 200 {
			t.Fatal("HTTPS 登录失败", status)
		}
		if sessions[index].Principal.UserID != user {
			t.Fatal("登录响应的所属用户错误")
		}
	}
	for _, session := range sessions {
		var response struct {
			Devices []identity.Device `json:"devices"`
		}
		if status := call("GET", "/api/v2/auth/devices", nil, session.AccessToken, &response); status != 200 {
			t.Fatal("设备查询失败", status)
		}
		if len(response.Devices) != 1 || response.Devices[0].ID != session.Principal.DeviceID {
			t.Fatal("HTTP 设备接口跨用户返回记录")
		}
	}
	if status := call("DELETE", "/api/v2/auth/devices/"+sessions[1].Principal.DeviceID, nil, sessions[0].AccessToken, nil); status != 403 {
		t.Fatal("HTTP 允许跨用户撤销设备", status)
	}
	var rotated identity.Session
	if status := call("POST", "/api/v2/auth/refresh", map[string]string{"refresh_token": string(sessions[0].RefreshToken)}, "", &rotated); status != 200 {
		t.Fatal("HTTP 续期失败", status)
	}
	if status := call("POST", "/api/v2/auth/refresh", map[string]string{"refresh_token": string(sessions[0].RefreshToken)}, "", nil); status != 401 {
		t.Fatal("HTTP 未拒绝旧续期值", status)
	}
	if status := call("GET", "/api/v2/auth/session", nil, rotated.AccessToken, nil); status != 401 {
		t.Fatal("HTTP 重放没有撤销会话", status)
	}
	if status := call("GET", "/api/v2/auth/session", nil, sessions[1].AccessToken, nil); status != 200 {
		t.Fatal("撤销影响了其他用户", status)
	}
}
