package nodefleet

import (
	"context"
	"database/sql"
	"errors"

	"log/slog"
	"net/http"
	"os"

	"time"

	"io.yueyang/tom/internal/httpjson"
	"io.yueyang/tom/internal/noderelease"
)

// Agent 只凭已登记节点的当前证书签名取本节点任务。分片序号绑定任务，不开放任意文件下载。
func (s Service) Agent(logger *slog.Logger) http.Handler {
	slots := make(chan struct{}, 32)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Origin") != "" {
			w.WriteHeader(400)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			w.WriteHeader(429)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		var signed noderelease.SignedCall
		if httpjson.Decode(r, &signed, 16<<10) != nil || !signed.Valid(time.Now()) {
			w.WriteHeader(403)
			return
		}
		c := signed.Call
		var job, release, state string
		err := s.DB.Platform(ctx, func(tx *sql.Tx) error {
			var pin string
			if err := tx.QueryRowContext(ctx, `SELECT certificate_fingerprint FROM tombot.node_connections WHERE node_id=$1`, c.Node).Scan(&pin); err != nil {
				return err
			}
			if pin != noderelease.Digest(signed.Certificate) {
				return ErrInput
			}
			if c.Action == "poll" {
				if _, err := tx.ExecContext(ctx, `INSERT INTO tombot.node_update_agents(node_id,platform) VALUES($1,$2) ON CONFLICT(node_id) DO UPDATE SET platform=$2,last_seen_at=clock_timestamp()`, c.Node, c.Platform); err != nil {
					return err
				}
				// 限制同时领取的大包数量；中断的节点再次轮询时复用同一任务。
				if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('tombot.fleet.claim',0))`); err != nil {
					return err
				}
				err := tx.QueryRowContext(ctx, `UPDATE tombot.node_update_jobs j SET state='running',updated_at=clock_timestamp() FROM tombot.node_releases r JOIN tombot.node_release_archives a USING(artifact) WHERE (a.manifest->>'version')::int <= $3 AND j.node_id=$1 AND j.release_id=r.id AND r.platform=$2 AND (j.state='running' OR (j.state='queued' AND (SELECT count(*) FROM tombot.node_update_jobs WHERE state='running' AND updated_at>clock_timestamp()-interval '4 hours')<3)) RETURNING j.id,j.release_id`, c.Node, c.Platform, max(1, c.PackageVersion)).Scan(&job, &release)
				if errors.Is(err, sql.ErrNoRows) {
					return nil
				}
				return err
			}
			if err := tx.QueryRowContext(ctx, `SELECT id,release_id,state FROM tombot.node_update_jobs WHERE node_id=$1 AND id=$2 FOR UPDATE`, c.Node, c.Job).Scan(&job, &release, &state); err != nil {
				return err
			}
			switch c.Action {
			case "part":
				if state != "running" {
					return ErrConflict
				}
				return nil
			case "report":
				if c.State != "started" && c.State != "failed" && c.State != "busy" {
					return ErrInput
				}
				if state == c.State {
					return nil
				}
				if state != "running" {
					return ErrConflict
				}
				_, err := tx.ExecContext(ctx, `UPDATE tombot.node_update_jobs SET state=$2,applied_image=CASE WHEN $2='started' THEN (SELECT image_id FROM tombot.node_connections WHERE node_id=$3) END,updated_at=clock_timestamp() WHERE id=$1`, job, c.State, c.Node)
				return err
			}
			return ErrInput
		})
		if err != nil {
			if errors.Is(err, ErrInput) || errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrConflict) {
				w.WriteHeader(403)
			} else {
				logger.Error("节点更新任务未确认", "node_id", c.Node, "reason", err)
				w.WriteHeader(503)
			}
			return
		}
		if job == "" || c.Action == "report" {
			w.WriteHeader(204)
			return
		}
		artifact, m, err := s.manifest(ctx, release)
		if err != nil {
			w.WriteHeader(503)
			return
		}
		if c.Action == "poll" {
			respond(w, 200, noderelease.Assignment{Job: job, Release: release, Artifact: artifact, Manifest: m}, logger)
			return
		}
		if c.Index < 0 || c.Index >= len(m.Chunks) {
			w.WriteHeader(400)
			return
		}
		d := m.Chunks[c.Index]
		part, err := os.Open(s.archivePath(artifact))
		if err != nil {
			w.WriteHeader(503)
			return
		}
		b := make([]byte, m.PartSize(c.Index))
		_, err = part.ReadAt(b, int64(c.Index)*noderelease.ChunkSize)
		err = errors.Join(err, part.Close())
		if err != nil || int64(len(b)) != m.PartSize(c.Index) || noderelease.Digest(b) != d {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if _, err = w.Write(b); err != nil {
			logger.Warn("升级分片传输中断", "node_id", c.Node, "job_id", job)
		}
	})
}
