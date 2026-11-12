package bots

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
)

const groupColumns = `g.id,g.name,g.purpose,g.max_cost_micro,c.id,g.is_archived,g.revision,g.created_at,g.updated_at,
	(SELECT COALESCE(jsonb_agg(m.bot_id ORDER BY m.bot_id),'[]'::jsonb) FROM
		(SELECT bot_id FROM tombot.group_members WHERE user_id=g.user_id AND group_id=g.id ORDER BY bot_id LIMIT 33) m)`
const groupJoin = ` FROM tombot.groups g JOIN tombot.conversations c ON c.user_id=g.user_id AND c.group_id=g.id AND c.kind='group' `

func scanGroup(row scanner) (Group, error) {
	var group Group
	var members []byte
	err := row.Scan(&group.ID, &group.Name, &group.Purpose, &group.MaxCostMicro, &group.ConversationID, &group.IsArchived, &group.Revision, &group.CreatedAt, &group.UpdatedAt, &members)
	if err != nil {
		return Group{}, err
	}
	if err = json.Unmarshal(members, &group.MemberIDs); err != nil {
		return Group{}, err
	}
	if len(group.MemberIDs) < 1 || len(group.MemberIDs) > 32 {
		return Group{}, errors.New("群组成员记录无效")
	}
	return group, nil
}
func readGroup(ctx context.Context, transaction *sql.Tx, userID, id string) (Group, error) {
	return scanGroup(transaction.QueryRowContext(ctx, `SELECT `+groupColumns+groupJoin+` WHERE g.user_id=$1 AND g.id=$2`, userID, id))
}
func listGroups(ctx context.Context, transaction *sql.Tx, userID, before string, limit int, archived bool) (result []Group, err error) {
	rows, err := transaction.QueryContext(ctx, `SELECT `+groupColumns+groupJoin+` WHERE g.user_id=$1 AND g.is_archived=$2 AND ($3='' OR g.id<$3) ORDER BY g.id DESC LIMIT $4`, userID, archived, before, limit)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	result = make([]Group, 0, limit)
	for rows.Next() {
		group, readErr := scanGroup(rows)
		if readErr != nil {
			return nil, readErr
		}
		result = append(result, group)
	}
	return result, rows.Err()
}

// checkMembers 以一次有界查询检查新增成员，已经加入的成员可以保留历史身份。
func checkMembers(ctx context.Context, transaction *sql.Tx, userID string, members, previous []string) (result error) {
	retained := make(map[string]bool, len(previous))
	for _, id := range previous {
		retained[id] = true
	}
	rows, err := transaction.QueryContext(ctx, `SELECT id,is_archived,COALESCE((messaging_policy->>'accept_group_invite')::boolean,false)
		FROM tombot.bots WHERE user_id=$1 AND id=ANY($2::text[]) ORDER BY id`, userID, members)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, rows.Close()) }()
	count := 0
	for rows.Next() {
		var id string
		var archived, accepts bool
		if err = rows.Scan(&id, &archived, &accepts); err != nil {
			return err
		}
		count++
		if !retained[id] && (archived || !accepts) {
			return ErrInviteDenied
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if count != len(members) {
		return ErrNotFound
	}
	return nil
}
func replaceMembers(ctx context.Context, transaction *sql.Tx, userID, groupID string, members []string) error {
	// 单事务中替换有界成员集合，任何失败均保留原成员，不逐个自动重试。
	if _, err := transaction.ExecContext(ctx, `DELETE FROM tombot.group_members WHERE user_id=$1 AND group_id=$2`, userID, groupID); err != nil {
		return err
	}
	_, err := transaction.ExecContext(ctx, `INSERT INTO tombot.group_members(user_id,group_id,bot_id) SELECT $1,$2,unnest($3::text[])`, userID, groupID, members)
	return err
}

// GetGroup 返回包含有界成员集合的群组详情，跨用户访问按不存在处理。
func (service *Service) GetGroup(ctx context.Context, principal identity.Principal, id string) (Group, error) {
	if !validID(id) {
		return Group{}, ErrInvalidInput
	}
	var group Group
	err := service.transaction(ctx, principal, false, func(transaction *sql.Tx) error {
		var err error
		group, err = readGroup(ctx, transaction, principal.UserID, id)
		return err
	})
	if err != nil {
		return Group{}, err
	}
	return group, nil
}

// ListGroups 一次查询返回目录页及成员身份，不逐群查询或返回聊天正文。
func (service *Service) ListGroups(ctx context.Context, principal identity.Principal, before string, limit int, archived bool) ([]Group, error) {
	if !validPage(before, limit) {
		return nil, ErrInvalidInput
	}
	var result []Group
	err := service.transaction(ctx, principal, false, func(transaction *sql.Tx) error {
		var err error
		result, err = listGroups(ctx, transaction, principal.UserID, before, limit, archived)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// CreateGroup 原子创建群组、成员和唯一会话；幂等键按用户及群组创建动作限定。
func (service *Service) CreateGroup(ctx context.Context, principal identity.Principal, request CreateGroupRequest) (Group, bool, error) {
	if !validID(request.ClientRequestID) {
		return Group{}, false, ErrInvalidInput
	}
	spec, err := normalizeGroup(request.GroupSpec)
	if err != nil {
		return Group{}, false, err
	}
	digest, err := requestDigest(spec)
	if err != nil {
		return Group{}, false, err
	}
	var group Group
	created := false
	err = service.transaction(ctx, principal, true, func(transaction *sql.Tx) error {
		var existing string
		var previous []byte
		err := transaction.QueryRowContext(ctx, `SELECT id,creation_digest FROM tombot.groups WHERE user_id=$1 AND client_request_id=$2`, principal.UserID, request.ClientRequestID).Scan(&existing, &previous)
		if err == nil {
			if !bytes.Equal(previous, digest) {
				return ErrConflict
			}
			group, err = readGroup(ctx, transaction, principal.UserID, existing)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var count int
		if err = transaction.QueryRowContext(ctx, `SELECT count(*) FROM tombot.groups WHERE user_id=$1`, principal.UserID).Scan(&count); err != nil {
			return err
		}
		if count >= service.options.MaxGroups {
			return ErrCapacity
		}
		if err = checkMembers(ctx, transaction, principal.UserID, spec.MemberIDs, nil); err != nil {
			return err
		}
		id := ids.New()
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.groups(user_id,id,name,purpose,max_cost_micro,client_request_id,creation_digest) VALUES($1,$2,$3,$4,$5,$6,$7)`, principal.UserID, id, spec.Name, spec.Purpose, spec.MaxCostMicro, request.ClientRequestID, digest); err != nil {
			return err
		}
		if err = replaceMembers(ctx, transaction, principal.UserID, id, spec.MemberIDs); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.conversations(user_id,id,kind,group_id,title) VALUES($1,$2,'group',$3,$4)`, principal.UserID, ids.New(), id, spec.Name); err != nil {
			return err
		}
		group, err = readGroup(ctx, transaction, principal.UserID, id)
		if err != nil {
			return err
		}
		if err = emitChange(ctx, transaction, principal.UserID, "group.created", id, group.Revision); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return Group{}, false, err
	}
	return group, created, nil
}

// UpdateGroup 将配置及成员作为一个资源修订更新；并发修改不会静默覆盖。
func (service *Service) UpdateGroup(ctx context.Context, principal identity.Principal, id string, request UpdateGroupRequest) (Group, error) {
	if !validID(id) || request.ExpectedRevision < 1 {
		return Group{}, ErrInvalidInput
	}
	spec, err := normalizeGroup(request.GroupSpec)
	if err != nil {
		return Group{}, err
	}
	var group Group
	err = service.transaction(ctx, principal, true, func(transaction *sql.Tx) error {
		current, err := readGroup(ctx, transaction, principal.UserID, id)
		if err != nil {
			return err
		}
		if current.Revision != request.ExpectedRevision {
			return ErrConflict
		}
		if current.IsArchived {
			return ErrArchived
		}
		if err = checkMembers(ctx, transaction, principal.UserID, spec.MemberIDs, current.MemberIDs); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.groups SET name=$3,purpose=$4,max_cost_micro=$5,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, principal.UserID, id, spec.Name, spec.Purpose, spec.MaxCostMicro); err != nil {
			return err
		}
		if err = replaceMembers(ctx, transaction, principal.UserID, id, spec.MemberIDs); err != nil {
			return err
		}
		if current.Name != spec.Name {
			if _, err = transaction.ExecContext(ctx, `UPDATE tombot.conversations SET title=$3,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND group_id=$2 AND kind='group'`, principal.UserID, id, spec.Name); err != nil {
				return err
			}
		}
		group, err = readGroup(ctx, transaction, principal.UserID, id)
		if err != nil {
			return err
		}
		return emitChange(ctx, transaction, principal.UserID, "group.updated", id, group.Revision)
	})
	if err != nil {
		return Group{}, err
	}
	return group, nil
}

// SetGroupArchived 归档群组及其会话，保留成员与历史供后续恢复。
func (service *Service) SetGroupArchived(ctx context.Context, principal identity.Principal, id string, revision int64, archived bool) (Group, error) {
	if !validID(id) || revision < 1 {
		return Group{}, ErrInvalidInput
	}
	var group Group
	err := service.transaction(ctx, principal, true, func(transaction *sql.Tx) error {
		var err error
		group, err = readGroup(ctx, transaction, principal.UserID, id)
		if err != nil {
			return err
		}
		if group.Revision != revision {
			return ErrConflict
		}
		if group.IsArchived == archived {
			return nil
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.groups SET is_archived=$3,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, principal.UserID, id, archived); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.conversations SET is_archived=$3,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND group_id=$2 AND kind='group'`, principal.UserID, id, archived); err != nil {
			return err
		}
		group, err = readGroup(ctx, transaction, principal.UserID, id)
		if err != nil {
			return err
		}
		return emitChange(ctx, transaction, principal.UserID, "group.updated", id, group.Revision)
	})
	if err != nil {
		return Group{}, err
	}
	return group, nil
}
