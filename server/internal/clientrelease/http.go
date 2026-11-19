package clientrelease

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/authapi"
	"io.yueyang/tom/internal/httpjson"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodecatalog"
	"io.yueyang/tom/internal/noderelease"
)

const adminPath = "/api/v2/admin/client-updates"
const publicPath = "/api/v2/client-updates"

type api struct {
	Service
	logger      *slog.Logger
	uploadSlots chan struct{}
}

// Register 只公开版本查询和已发布的分片，写操作共用管理会话、CSRF 与敏感操作再验证。
func (s Service) Register(router *http.ServeMux, gate *authapi.Handler, auth nodecatalog.Authorizer, logger *slog.Logger) error {
	a := api{s, logger, make(chan struct{}, 8)}
	admin, err := gate.Protect("admin", func(w http.ResponseWriter, r *http.Request, p identity.Principal, attempt identity.Attempt) {
		if err := auth.RequireAdmin(r.Context(), p, r.Method != http.MethodGet); err != nil {
			if errors.Is(err, identity.ErrVerificationRequired) {
				a.fail(w, r, 403, "VERIFICATION_REQUIRED", "请重新验证管理员身份")
			} else {
				a.fail(w, r, 403, "FORBIDDEN", "需要管理员身份")
			}
			return
		}
		a.admin(w, r, attempt, p.UserID)
	})
	if err != nil {
		return err
	}
	router.Handle(adminPath, admin)
	router.Handle(adminPath+"/", admin)
	public := http.NewServeMux()
	public.HandleFunc("GET "+publicPath+"/latest", a.check)
	public.HandleFunc("GET "+publicPath+"/{id}/parts/{index}", a.part)
	slots := make(chan struct{}, 32)
	router.HandleFunc(publicPath+"/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", ids.New())
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			public.ServeHTTP(w, r)
		default:
			a.fail(w, r, 429, "RELEASE_BUSY", "客户端升级服务繁忙，请稍后重试")
		}
	})
	return nil
}

// json 为升级查询和发布回执禁用缓存，客户端每次核对都能看到新发布。
func (a api) json(w http.ResponseWriter, r *http.Request, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		a.logger.WarnContext(r.Context(), "客户端升级响应未发送完成", "request_id", w.Header().Get("X-Request-ID"), "status", status)
	}
}

func (a api) fail(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	a.json(w, r, status, map[string]string{"code": code, "message": message, "request_id": w.Header().Get("X-Request-ID")})
}

// failure 仅暴露固定的业务错误，不将驱动诊断或本地路径发送给客户端。
func (a api) failure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errInput):
		a.fail(w, r, 400, "INPUT_INVALID", errInput.Error())
	case errors.Is(err, errDuplicate), errors.Is(err, errIncomplete):
		a.fail(w, r, 409, "RELEASE_CONFLICT", err.Error())
	case errors.Is(err, sql.ErrNoRows):
		a.fail(w, r, 404, "NOT_FOUND", "发布包或分片不存在")
	default:
		a.logger.ErrorContext(r.Context(), "客户端升级存储操作失败", "request_id", w.Header().Get("X-Request-ID"), "release_id", r.PathValue("id"), "method", r.Method)
		a.fail(w, r, 503, "RELEASE_UNAVAILABLE", "客户端升级服务暂不可用，请检查发布存储")
	}
}

// admin 处理已授权的发布操作，分片传输与发布事务分开以适应 HTTP 请求时限。
func (a api) admin(w http.ResponseWriter, r *http.Request, attempt identity.Attempt, actor string) {
	path := strings.TrimPrefix(r.URL.Path, adminPath)
	switch {
	case r.Method == "GET" && path == "":
		releases, err := a.list(r.Context())
		if err != nil {
			a.failure(w, r, err)
			return
		}
		a.json(w, r, 200, map[string]any{"releases": releases, "platforms": platforms, "chunk_size": chunkSize, "max_size": maxSize})
	case r.Method == "PUT" && strings.HasPrefix(path, "/parts/"):
		digest := strings.TrimPrefix(path, "/parts/")
		if !noderelease.ValidDigest(digest) || r.Header.Get("Content-Type") != "application/octet-stream" {
			a.failure(w, r, errInput)
			return
		}
		select {
		case a.uploadSlots <- struct{}{}:
			defer func() { <-a.uploadSlots }()
		default:
			a.fail(w, r, 429, "UPLOAD_BUSY", "上传繁忙，请稍后重试")
			return
		}
		part, err := io.ReadAll(http.MaxBytesReader(w, r.Body, chunkSize))
		if err != nil {
			a.fail(w, r, 400, "INVALID_PART", "分片不完整或超过大小限制")
			return
		}
		if err = a.savePart(digest, part); err != nil {
			a.failure(w, r, err)
			return
		}
		a.json(w, r, 200, struct{}{})
	case r.Method == "POST" && path == "":
		var m manifest
		if f := httpjson.Decode(r, &m, 64<<10); f != nil {
			a.fail(w, r, f.Status, f.Code, f.Message)
			return
		}
		id, err := a.publish(r.Context(), m, actor, attempt.RequestID)
		if err != nil {
			a.failure(w, r, err)
			return
		}
		a.logger.InfoContext(r.Context(), "已发布客户端升级", "release_id", id, "platform", m.Platform, "version", m.Version, "actor_id", actor)
		a.json(w, r, 201, map[string]string{"id": id})
	default:
		a.fail(w, r, 404, "NOT_FOUND", "升级接口不存在")
	}
}

// check 返回当前安装版本可用的更新，空结果也要与服务异常区分。
func (a api) check(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	latest, err := a.latest(r.Context(), q.Get("platform"), q.Get("version"))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	a.json(w, r, 200, map[string]any{"release": latest})
}

// part 由不可变的发布记录解析分片；未发布的暂存文件不能经公共接口读取。
func (a api) part(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := ulid.ParseStrict(id)
	i, indexErr := strconv.Atoi(r.PathValue("index"))
	if err != nil || indexErr != nil || i < 0 || i >= maxSize/chunkSize {
		a.failure(w, r, errInput)
		return
	}
	var digest string
	err = a.DB.Platform(r.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.Context(), `SELECT manifest->'chunks'->>$2::int FROM tombot.client_releases WHERE id=$1 AND jsonb_array_length(manifest->'chunks')>$2::int`, id, i).Scan(&digest)
	})
	if err != nil {
		a.failure(w, r, err)
		return
	}
	if !noderelease.ValidDigest(digest) {
		a.failure(w, r, errIncomplete)
		return
	}
	file, err := os.Open(filepath.Join(a.Directory, digest))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	defer func() {
		if err := file.Close(); err != nil {
			a.logger.WarnContext(r.Context(), "关闭客户端安装包分片失败", "release_id", id, "part", i)
		}
	}()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() <= 0 || stat.Size() > chunkSize {
		a.failure(w, r, errIncomplete)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	if r.Method != "HEAD" {
		if _, err := io.CopyN(w, file, stat.Size()); err != nil {
			a.logger.WarnContext(r.Context(), "客户端安装包分片传输中断", "release_id", id, "part", i)
		}
	}
}
