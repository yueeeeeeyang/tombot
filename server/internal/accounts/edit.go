package accounts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
)

var ErrEditNodeUnavailable = errors.New("请确认源节点和目标节点均在线，且已安装支持用户电脑编辑的 tomnode 版本")
var ErrEditConflict = errors.New("用户状态或配置已变化，或电脑有未完成的操作，请重新读取")
var ErrUserMissing = errors.New("用户不存在")

type UserLimits struct {
	Models   int    `json:"concurrent_model_calls"`
	Desktops int    `json:"concurrent_desktops"`
	Queued   int    `json:"queued_runs"`
	Monthly  *int64 `json:"monthly_token_limit,string"`
}
type ComputerSettings struct {
	NodeID    string             `json:"node_id"`
	ImageID   string             `json:"image_id"`
	Resources computer.Resources `json:"resources"`
}
type EditProgress struct {
	Stage   string           `json:"stage"`
	Copied  int64            `json:"copied"`
	Total   int64            `json:"total"`
	Failure *string          `json:"failure"`
	Paused  bool             `json:"paused"`
	Target  ComputerSettings `json:"target"`
}
type UserSettings struct {
	Username       string           `json:"username"`
	DisplayName    string           `json:"display_name"`
	NodeName       string           `json:"node_name"`
	ImageReference string           `json:"image_reference"`
	HasAvatar      bool             `json:"has_avatar"`
	Revision       int64            `json:"revision"`
	Generation     int64            `json:"generation"`
	Limits         UserLimits       `json:"limits"`
	Computer       ComputerSettings `json:"computer"`
	Progress       *EditProgress    `json:"progress"`
}
type UserEdit struct {
	Revision     int64            `json:"revision"`
	Generation   int64            `json:"generation"`
	DisplayName  string           `json:"display_name"`
	Avatar       []byte           `json:"avatar,omitempty"`
	RemoveAvatar bool             `json:"remove_avatar,omitempty"`
	Limits       UserLimits       `json:"limits"`
	Computer     ComputerSettings `json:"computer"`
}

func validEditID(id string) bool {
	parsed, err := ulid.ParseStrict(id)
	return err == nil && parsed.String() == id
}

func readUserSettings(ctx context.Context, tx *sql.Tx, userID string, lock bool) (v UserSettings, err error) {
	var status string
	query := `SELECT u.username,u.display_name,u.revision,u.status,EXISTS(SELECT 1 FROM tombot.user_avatars a WHERE a.user_id=u.id),
 l.concurrent_model_calls,l.concurrent_desktops,l.queued_runs,l.monthly_token_limit,c.node_id,c.image_id,n.name,i.reference,c.generation,c.cpu_millis,c.memory_bytes,c.disk_bytes,c.process_limit
 FROM tombot.users u JOIN tombot.user_limits l ON l.user_id=u.id JOIN tombot.computers c ON c.user_id=u.id JOIN tombot.computer_nodes n ON n.id=c.node_id JOIN tombot.computer_images i ON i.id=c.image_id WHERE u.id=$1`
	if lock {
		query += " FOR UPDATE OF u,l,c"
	}
	r := &v.Computer.Resources
	err = tx.QueryRowContext(ctx, query, userID).Scan(&v.Username, &v.DisplayName, &v.Revision, &status, &v.HasAvatar, &v.Limits.Models, &v.Limits.Desktops, &v.Limits.Queued, &v.Limits.Monthly, &v.Computer.NodeID, &v.Computer.ImageID, &v.NodeName, &v.ImageReference, &v.Generation, &r.CPUMillis, &r.MemoryBytes, &r.DiskBytes, &r.ProcessLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrUserMissing
	}
	if err != nil {
		return v, err
	}
	if status != "disabled" {
		return v, ErrEditConflict
	}
	var progress EditProgress
	var spec []byte
	err = tx.QueryRowContext(ctx, `SELECT stage,copied,(source_spec->'resources'->>'disk_bytes')::bigint,failure,attempts >= $2,target_image_id,target_spec FROM tombot.computer_edits WHERE user_id=$1 AND stage<>'succeeded'`, userID, maxEditAttempts).Scan(&progress.Stage, &progress.Copied, &progress.Total, &progress.Failure, &progress.Paused, &progress.Target.ImageID, &spec)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	var target computer.Specification
	if err = json.Unmarshal(spec, &target); err != nil {
		return v, err
	}
	progress.Target.NodeID, progress.Target.Resources = target.NodeID, target.Resources
	v.Progress = &progress
	return v, nil
}

// UserSettings 读取禁用用户当前配置和迁移进度，不返回头像或工作区内容。
func (s *Service) UserSettings(ctx context.Context, p identity.Principal, id string) (v UserSettings, err error) {
	if err = s.authorizer.RequireAdmin(ctx, p, false); err != nil {
		return
	}
	if !validEditID(id) {
		return v, ErrInvalidInput
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		var readErr error
		v, readErr = readUserSettings(ctx, tx, id, false)
		return readErr
	})
	return
}

// EditUser 复验禁用状态及页面版本，在同一事务保存资料、额度、目标资源预留和审计。
func (s *Service) EditUser(ctx context.Context, p identity.Principal, id, requestID string, in UserEdit) (UserSettings, error) {
	if err := s.authorizer.RequireAdmin(ctx, p, true); err != nil {
		return UserSettings{}, err
	}
	name, err := identity.NormalizeDisplayName(in.DisplayName)
	l, c := in.Limits, in.Computer
	if err != nil || !validEditID(id) || !validEditID(requestID) || in.Revision < 1 || in.Generation < 1 || l.Models < 1 || l.Models > 100 || l.Desktops < 1 || l.Desktops > computer.MaxConcurrentDesktops || l.Queued < 1 || l.Queued > 10000 || l.Monthly != nil && *l.Monthly < 1 || !validEditID(c.NodeID) || !validEditID(c.ImageID) || c.Resources.Validate() != nil || in.RemoveAvatar && in.Avatar != nil {
		return UserSettings{}, ErrInvalidInput
	}
	if in.Avatar != nil {
		in.Avatar, err = identity.NormalizeUserAvatar(in.Avatar)
		if err != nil {
			return UserSettings{}, err
		}
	}
	// 只在电脑确有变化时探测协议，资料修改不依赖节点在线。写事务内仍复验同一版本和节点。
	var original UserSettings
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		var readErr error
		original, readErr = readUserSettings(ctx, tx, id, false)
		return readErr
	})
	if err != nil {
		return UserSettings{}, err
	}
	if original.Revision != in.Revision || original.Generation != in.Generation {
		return UserSettings{}, ErrEditConflict
	}
	changesComputer := original.Progress == nil && original.Computer != c
	if changesComputer {
		nodes, ok := s.runtime.(computer.EditSupport)
		if !ok {
			return UserSettings{}, ErrEditNodeUnavailable
		}
		for _, node := range uniqueEditNodes(original.Computer.NodeID, c.NodeID) {
			bounded, cancel := context.WithTimeout(ctx, s.options.CallTimeout)
			err = nodes.CheckEditSupport(bounded, computer.NodeQuery{NodeID: node})
			cancel()
			if err != nil {
				return UserSettings{}, ErrEditNodeUnavailable
			}
		}
	}
	var result UserSettings
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if changesComputer || original.Progress != nil && original.Progress.Paused {
			// 与会话准入和自动镜像升级使用相同顺序，防止空闲检查之后再签发旧实例。
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended('tombot.node-maintenance/'||n.id,0)) FROM tombot.computer_nodes n WHERE n.id=$2 OR n.id=(SELECT node_id FROM tombot.computers WHERE user_id=$1) ORDER BY n.id`, id, c.NodeID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO tombot.computer_execution_heads(user_id,computer_id) SELECT user_id,id FROM tombot.computers WHERE user_id=$1 ON CONFLICT(user_id) DO NOTHING`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `SELECT user_id FROM tombot.computer_execution_heads WHERE user_id=$1 FOR UPDATE`, id); err != nil {
				return err
			}
		}
		current, err := readUserSettings(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if current.Revision != in.Revision || current.Generation != in.Generation {
			return ErrEditConflict
		}
		if current.Progress != nil {
			if c != current.Progress.Target {
				return ErrEditConflict
			}
			if current.Progress.Paused {
				err = tx.QueryRowContext(ctx, `UPDATE tombot.computer_edits e SET attempts=0,next_attempt_at=clock_timestamp() WHERE user_id=$1
 AND NOT EXISTS(SELECT 1 FROM tombot.node_upgrades m WHERE NOT m.is_finished AND m.node_id IN (e.source_spec->>'node_id',e.target_spec->>'node_id')) RETURNING user_id`, id).Scan(&id)
				if errors.Is(err, sql.ErrNoRows) {
					return ErrEditConflict
				}
				if err != nil {
					return err
				}
			}
		} else if c != current.Computer {
			if err = planComputerEdit(ctx, tx, id, current, c); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tombot.users SET display_name=$2,revision=revision+1,updated_at=clock_timestamp() WHERE id=$1`, id, name); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tombot.user_limits SET concurrent_model_calls=$2,concurrent_desktops=$3,queued_runs=$4,monthly_token_limit=$5,revision=revision+1 WHERE user_id=$1`, id, l.Models, l.Desktops, l.Queued, l.Monthly); err != nil {
			return err
		}
		if in.RemoveAvatar {
			_, err = tx.ExecContext(ctx, `DELETE FROM tombot.user_avatars WHERE user_id=$1`, id)
		} else if in.Avatar != nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO tombot.user_avatars(user_id,content) VALUES($1,$2) ON CONFLICT(user_id) DO UPDATE SET content=$2,updated_at=clock_timestamp()`, id, in.Avatar)
		}
		if err != nil {
			return err
		}
		if err = audit(ctx, tx, p.UserID, id, "编辑禁用用户", requestID, "succeeded", map[string]any{"revision": in.Revision + 1, "node_id": c.NodeID, "image_id": c.ImageID}); err != nil {
			return err
		}
		result, err = readUserSettings(ctx, tx, id, false)
		return err
	})
	return result, err
}

func planComputerEdit(ctx context.Context, tx *sql.Tx, user string, current UserSettings, settings ComputerSettings) error {
	var source computer.Specification
	var previous []byte
	var container, targetImage string
	err := tx.QueryRowContext(ctx, `SELECT c.id,c.container_id,c.volume_ref,i.reference,o.id,o.attempt,o.fencing_token,o.updated_at,
 COALESCE((SELECT target_specification FROM tombot.computer_image_upgrades p WHERE p.user_id=c.user_id AND p.state='succeeded' AND (target_specification->>'generation')::bigint=c.generation),
 (SELECT target_spec FROM tombot.computer_edits e WHERE e.user_id=c.user_id AND e.stage='succeeded' AND (target_spec->>'generation')::bigint=c.generation))
 FROM tombot.computers c JOIN tombot.user_creation_operations o ON o.user_id=c.user_id JOIN tombot.computer_images i ON i.id=c.image_id
 WHERE c.user_id=$1 AND c.status='ready' AND o.status='succeeded'
 AND NOT EXISTS(SELECT 1 FROM tombot.computer_sessions s WHERE s.user_id=$1 AND s.status<>'released')
 AND NOT EXISTS(SELECT 1 FROM tombot.computer_image_upgrades p WHERE p.user_id=$1 AND p.state<>'succeeded')`, user).Scan(&source.ComputerID, &container, &source.VolumeReference, &source.ImageReference, &source.OperationID, &source.Attempt, &source.FencingToken, &source.LeaseExpiresAt, &previous)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEditConflict
	}
	if err != nil {
		return err
	}
	source.UserID, source.NodeID, source.Generation, source.Resources = user, current.Computer.NodeID, current.Generation, current.Computer.Resources
	if len(previous) > 0 {
		var prior computer.Specification
		if err = json.Unmarshal(previous, &prior); err != nil {
			return err
		}
		source.Attempt, source.FencingToken, source.LeaseExpiresAt = prior.Attempt, prior.FencingToken, prior.LeaseExpiresAt
		if prior != source {
			return ErrEditConflict
		}
	}
	if err = source.Validate(); err != nil {
		return ErrEditConflict
	}
	// 同节点已占用当前名额。跨节点只在目标已验收且确有空位时新增预留。
	if settings.NodeID != source.NodeID {
		var reserved string
		err = tx.QueryRowContext(ctx, `SELECT tombot.reserve_computer_node($1,$2)`, settings.NodeID, settings.ImageID).Scan(&reserved)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoCapacity
		}
		if err != nil {
			return err
		}
	}
	err = tx.QueryRowContext(ctx, `SELECT i.reference FROM tombot.computer_nodes n JOIN tombot.computer_images i ON i.id=n.verified_image_id AND i.is_enabled
 WHERE n.id=$1 AND i.id=$2 AND tombot.computer_node_status(n.status,n.runtime_id,n.control_expires_at,n.capability_expires_at)='ready'
 AND NOT EXISTS(SELECT 1 FROM tombot.node_upgrades WHERE node_id IN ($1,$3) AND NOT is_finished) FOR SHARE OF n,i`, settings.NodeID, settings.ImageID, source.NodeID).Scan(&targetImage)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrImageUnavailable
	}
	if err != nil {
		return err
	}
	target := source
	target.Generation++
	target.Attempt++
	target.FencingToken++
	target.Resources = settings.Resources
	target.NodeID = settings.NodeID
	target.ImageReference = targetImage
	from, err := json.Marshal(source)
	if err != nil {
		return err
	}
	to, err := json.Marshal(target)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tombot.computer_edits(user_id,computer_id,edit_id,source_image_id,target_image_id,source_spec,target_spec,source_container,stage)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,'checking') ON CONFLICT(user_id) DO UPDATE SET edit_id=excluded.edit_id,source_image_id=$4,target_image_id=$5,source_spec=$6,target_spec=$7,source_container=$8,stage='checking',attempts=0,copied=0,failure=NULL,lease_until=NULL,next_attempt_at=clock_timestamp()`, user, source.ComputerID, ids.New(), current.Computer.ImageID, settings.ImageID, from, to, container)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE tombot.computers SET status='stopping',updated_at=clock_timestamp() WHERE user_id=$1`, user)
	return err
}

// SetUserEnabled 只切换已激活普通用户。认证版本递增，重新启用后旧会话仍失效。
func (s *Service) SetUserEnabled(ctx context.Context, actor identity.Principal, user, request string, enabled bool, revision int64) error {
	if err := s.authorizer.RequireAdmin(ctx, actor, true); err != nil {
		return err
	}
	if !validEditID(user) || !validEditID(request) || revision < 1 {
		return ErrInvalidInput
	}
	from, to := "active", "disabled"
	if enabled {
		from, to = to, from
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var locked string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM tombot.users WHERE id=$1 AND revision=$2 FOR UPDATE`, user, revision).Scan(&locked); errors.Is(err, sql.ErrNoRows) || err == nil && locked != from {
			return ErrEditConflict
		} else if err != nil {
			return err
		}
		r, err := tx.ExecContext(ctx, `UPDATE tombot.users SET status=$2,auth_version=auth_version+1,revision=revision+1,updated_at=clock_timestamp() WHERE id=$1 AND NOT EXISTS(SELECT 1 FROM tombot.computer_edits WHERE user_id=$1 AND stage<>'succeeded')`, user, to)
		if err != nil {
			return err
		}
		count, err := r.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrEditConflict
		}
		return audit(ctx, tx, actor.UserID, user, "变更用户启用状态", request, "succeeded", map[string]any{"status": to})
	})
}

func uniqueEditNodes(source, target string) []string {
	if source == target {
		return []string{source}
	}
	return []string{source, target}
}
