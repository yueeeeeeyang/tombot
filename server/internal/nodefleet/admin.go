package nodefleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"io.yueyang/tom/internal/authapi"
	"io.yueyang/tom/internal/httpjson"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/nodecatalog"
	"io.yueyang/tom/internal/noderelease"
)

const AdminPath = "/api/v2/admin/node-updates/"

func respond(w http.ResponseWriter, status int, body any, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		logger.Warn("节点升级回执发送失败")
	}
}
func fail(w http.ResponseWriter, status int, code, msg string, logger *slog.Logger) {
	respond(w, status, map[string]string{"code": code, "message": msg}, logger)
}
func (s Service) Admin(gate *authapi.Handler, auth nodecatalog.Authorizer, logger *slog.Logger) (http.Handler, error) {
	uploadSlots := make(chan struct{}, 8)
	verifySlots := make(chan struct{}, 2)
	return gate.Protect("admin", func(w http.ResponseWriter, r *http.Request, p identity.Principal, a identity.Attempt) {
		ctx := r.Context()
		if err := auth.RequireAdmin(ctx, p, r.Method != "GET"); err != nil {
			if errors.Is(err, identity.ErrVerificationRequired) {
				fail(w, 403, "VERIFICATION_REQUIRED", err.Error(), logger)
			} else {
				fail(w, 403, "FORBIDDEN", "需要管理员身份", logger)
			}
			return
		}
		var result any = struct{}{}
		var err error
		path := strings.TrimPrefix(r.URL.Path, AdminPath)
		switch {
		case r.Method == "GET" && path == "status":
			result, err = s.status(ctx)
		case r.Method == "POST" && path == "uploads":
			var input struct {
				Artifact string `json:"artifact"`
				Size     int64  `json:"size"`
			}
			if httpjson.Decode(r, &input, 1024) != nil {
				err = ErrInput
			} else {
				result, err = s.begin(p.UserID, input.Artifact, input.Size)
			}
		case r.Method == "POST" && path == "releases":
			var input struct {
				Artifact string `json:"artifact"`
			}
			if httpjson.Decode(r, &input, 1024) != nil {
				err = ErrInput
				break
			}
			select {
			case verifySlots <- struct{}{}:
				defer func() { <-verifySlots }()
			default:
				fail(w, 429, "UPDATE_BUSY", "正在校验其他升级包，请稍后重试", logger)
				return
			}
			deadline, ok := ctx.Deadline()
			if !ok {
				err = errors.New("升级校验缺少请求时限")
				break
			}
			if err = http.NewResponseController(w).SetWriteDeadline(deadline.Add(time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
				break
			}
			result, err = s.publish(ctx, p.UserID, input.Artifact)
			if err == nil {
				logger.Info("节点升级包已校验", "actor_id", p.UserID, "request_id", a.RequestID, "artifact", input.Artifact)
			}
		case r.Method == "PUT" && strings.HasPrefix(path, "parts/"):
			select {
			case uploadSlots <- struct{}{}:
				defer func() { <-uploadSlots }()
			default:
				fail(w, 429, "UPDATE_BUSY", "同时上传的分片过多，请稍后重试", logger)
				return
			}
			parts := strings.Split(path, "/")
			if len(parts) != 4 || r.Header.Get("Content-Type") != "application/octet-stream" {
				err = ErrInput
				break
			}
			var b []byte
			b, err = io.ReadAll(http.MaxBytesReader(w, r.Body, noderelease.ChunkSize))
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				err = ErrInput
			}
			if err == nil {
				err = s.savePart(p.UserID, parts[1], parts[2], parts[3], b)
			}
		case r.Method == "POST" && path == "dispatch":
			var body struct {
				All     bool     `json:"all"`
				Release string   `json:"release"`
				Nodes   []string `json:"nodes"`
			}
			if httpjson.Decode(r, &body, 16<<10) != nil {
				err = ErrInput
			} else {
				err = s.dispatch(ctx, body.Release, body.Nodes, body.All, p.UserID, a.RequestID)
			}
		default:
			fail(w, 404, "NOT_FOUND", "升级接口不存在", logger)
			return
		}
		if err != nil {
			switch {
			case errors.Is(err, noderelease.ErrPackage):
				fail(w, 400, "PACKAGE_INVALID", "离线包校验失败，请选择完整的标准节点升级包", logger)
			case errors.Is(err, ErrUploadBusy):
				fail(w, 409, "UPDATE_BUSY", ErrUploadBusy.Error(), logger)
			case errors.Is(err, ErrInput):
				fail(w, 400, "INPUT_INVALID", ErrInput.Error(), logger)
			case errors.Is(err, ErrConflict), errors.Is(err, sql.ErrNoRows):
				fail(w, 409, "UPDATE_CONFLICT", ErrConflict.Error(), logger)
			default:
				logger.Error("节点发布操作失败", "request_id", a.RequestID, "action", r.Method, "reason", err)
				fail(w, 503, "UPDATE_UNAVAILABLE", "升级服务不可用，请检查存储或服务端日志", logger)
			}
			return
		}
		respond(w, 200, result, logger)
	})
}

type dashboard struct {
	Releases []Release `json:"releases"`
	Jobs     []Job     `json:"jobs"`
}

func (s Service) status(ctx context.Context) (dashboard, error) {
	value := dashboard{[]Release{}, []Job{}}
	err := s.DB.Platform(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id,label,platform FROM tombot.node_releases JOIN tombot.node_release_archives USING(artifact) ORDER BY created_at DESC LIMIT 50`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Release
			if err := rows.Scan(&r.ID, &r.Label, &r.Platform); err != nil {
				return err
			}
			value.Releases = append(value.Releases, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, `SELECT j.id,j.node_id,CASE WHEN j.state='started' AND c.image_id<>j.applied_image THEN 'superseded' WHEN j.state='queued' AND a.last_seen_at<clock_timestamp()-interval '3 minutes' THEN 'offline' WHEN j.state='started' AND n.verified_image_id=c.image_id AND tombot.computer_node_status(n.status,n.runtime_id,n.control_expires_at,n.capability_expires_at)='ready' THEN 'ready' WHEN j.state='started' AND v.image_id=c.image_id AND v.state IN ('failed','quarantined') AND v.updated_at>=j.updated_at THEN 'verification_failed' ELSE j.state END,r.label FROM tombot.node_update_jobs j LEFT JOIN tombot.node_update_agents a ON a.node_id=j.node_id JOIN tombot.node_releases r ON r.id=j.release_id JOIN tombot.computer_nodes n ON n.id=j.node_id JOIN tombot.node_connections c ON c.node_id=j.node_id LEFT JOIN tombot.node_verifications v ON v.node_id=j.node_id ORDER BY j.updated_at DESC LIMIT 1000`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var j Job
			if err := rows.Scan(&j.ID, &j.Node, &j.State, &j.Label); err != nil {
				return err
			}
			value.Jobs = append(value.Jobs, j)
		}
		return rows.Err()
	})
	return value, err
}
