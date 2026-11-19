// Package nodefleet 管理由管理员上传并通过校验的节点包和每节点的升级任务，不接收命令、下载地址或宿主路径。
package nodefleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/noderelease"
	"io.yueyang/tom/internal/store/postgres"
)

const Directory = "/var/lib/tombot-node-releases"

type Service struct {
	DB        *postgres.Database
	Directory string
}

var ErrUploadBusy = errors.New("该账号有另一项上传正在处理，请稍后重试")
var ErrInput = errors.New("发布参数或分片摘要无效")
var ErrConflict = errors.New("发布包尚未上传完成、节点不存在或已有升级任务")

type Release struct {
	Kind     string `json:"kind,omitempty"`
	ID       string `json:"id"`
	Label    string `json:"label"`
	Platform string `json:"platform"`
}
type Job struct {
	ID    string `json:"id"`
	Node  string `json:"node"`
	State string `json:"state"`
	Label string `json:"label"`
}

func (s Service) manifest(ctx context.Context, id string) (artifact string, m noderelease.Manifest, err error) {
	if !noderelease.ValidDigest(id) {
		return "", m, ErrInput
	}
	var encoded []byte
	err = s.DB.Platform(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT r.artifact,a.manifest FROM tombot.node_releases r JOIN tombot.node_release_archives a USING(artifact) WHERE r.id=$1`, id).Scan(&artifact, &encoded)
	})
	if err != nil {
		return "", m, err
	}
	if err = json.Unmarshal(encoded, &m); err != nil {
		return "", m, err
	}
	return artifact, m, m.Validate(artifact)
}

func (s Service) dispatch(ctx context.Context, id string, nodes []string, all bool, actor, requestID string) error {
	artifact, m, err := s.manifest(ctx, id)
	if err != nil {
		return err
	}
	if (len(nodes) == 0) != all || len(nodes) > 100 {
		return ErrInput
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		if seen[n] || (computer.NodeQuery{NodeID: n}).Validate() != nil {
			return ErrInput
		}
		seen[n] = true
	}
	info, err := os.Lstat(s.archivePath(artifact))
	if err != nil || !info.Mode().IsRegular() || info.Size() != m.Size {
		return ErrConflict
	}
	return s.DB.Platform(ctx, func(tx *sql.Tx) error {
		// 按固定节点顺序协调并发下发；实际分配和验收在节点申请维护时封闭。
		rows, err := tx.QueryContext(ctx, `SELECT n.id,coalesce(a.platform,'') FROM tombot.computer_nodes n
			JOIN tombot.node_connections c ON c.node_id=n.id LEFT JOIN tombot.node_update_agents a ON a.node_id=n.id
			WHERE ($1 AND a.platform=$2) OR n.id=ANY($3::text[]) ORDER BY n.id LIMIT 1001 FOR UPDATE OF n`, all, m.Platform, nodes)
		if err != nil {
			return err
		}
		defer rows.Close()
		targets, operations, states := []string{}, []string{}, []string{}
		for rows.Next() {
			var node, platform string
			if err := rows.Scan(&node, &platform); err != nil {
				return err
			}
			state := "queued"
			if platform != m.Platform {
				state = "unsupported"
			}
			targets = append(targets, node)
			operations = append(operations, ids.New())
			states = append(states, state)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(targets) == 0 || len(targets) > 1000 || !all && len(targets) != len(nodes) {
			return ErrConflict
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO tombot.node_update_jobs(node_id,id,release_id,state)
			SELECT target,operation,$1,state FROM unnest($2::text[],$3::text[],$4::text[]) AS batch(target,operation,state)
			ON CONFLICT(node_id) DO UPDATE SET id=EXCLUDED.id,release_id=EXCLUDED.release_id,state=EXCLUDED.state,applied_image=NULL,updated_at=clock_timestamp()
			WHERE tombot.node_update_jobs.state NOT IN ('queued','running')`, id, targets, operations, states)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		// 已有在途任务的节点不会计入更新行数，整批回滚以保持任务与审计一致。
		if count != int64(len(targets)) {
			return ErrConflict
		}
		changes, err := json.Marshal(map[string]any{"release_id": id, "nodes": targets})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO tombot.admin_audit_events(id,actor_id,action,target_kind,target_id,request_id,result,changes) VALUES($1,$2,'下发节点升级','computer_node',$3,$4,'succeeded',$5)`, ids.New(), actor, targets[0], requestID, changes)
		return err
	})
}
