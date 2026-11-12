package accounts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
)

func normalizeRequest(request CreateRequest) (CreateRequest, error) {
	username, err := identity.NormalizeUsername(request.Username)
	if err != nil {
		return request, ErrInvalidInput
	}
	request.Username = username
	request.DisplayName = strings.TrimSpace(request.DisplayName)
	if request.DisplayName == "" || !utf8.ValidString(request.DisplayName) || utf8.RuneCountInString(request.DisplayName) > 120 || strings.ContainsAny(request.DisplayName, "\r\n\x00") || request.Role != "admin" && request.Role != "user" || len(request.IdempotencyKey) < 1 || len(request.IdempotencyKey) > 120 {
		return request, ErrInvalidInput
	}
	for _, character := range request.IdempotencyKey {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("-_.:", character)) {
			return request, ErrInvalidInput
		}
	}
	for _, id := range []string{request.NodeID, request.ImageID, request.RequestID} {
		if _, err := ulid.ParseStrict(id); err != nil {
			return request, ErrInvalidInput
		}
	}
	if err := request.Resources.Validate(); err != nil {
		return request, ErrInvalidInput
	}
	return request, nil
}

func (service *Service) reserve(ctx context.Context, actorID string, request CreateRequest, isBootstrap bool) (Operation, error) {
	request, err := normalizeRequest(request)
	if err != nil {
		return Operation{}, err
	}
	if !isBootstrap {
		if _, err := ulid.ParseStrict(actorID); err != nil {
			return Operation{}, ErrInvalidInput
		}
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return Operation{}, ErrInvalidInput
	}
	digest := sha256.Sum256(encoded)
	var operation Operation
	err = service.transaction(ctx, func(transaction *sql.Tx) error {
		if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('tombot.user_creation.' || $1,0))`, request.IdempotencyKey); err != nil {
			return err
		}
		var previousDigest []byte
		var previousActor sql.NullString
		err := transaction.QueryRowContext(ctx, `SELECT id,user_id,computer_id,status,stage,attempt,error_code,created_at,updated_at,request_digest,actor_id
			FROM tombot.user_creation_operations WHERE idempotency_key=$1`, request.IdempotencyKey).
			Scan(&operation.ID, &operation.UserID, &operation.ComputerID, &operation.Status, &operation.Stage, &operation.Attempt, &operation.ErrorCode, &operation.CreatedAt, &operation.UpdatedAt, &previousDigest, &previousActor)
		if err == nil {
			if !bytes.Equal(previousDigest, digest[:]) || previousActor.String != actorID {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if isBootstrap {
			if _, err = transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('tombot.bootstrap.users',0))`); err != nil {
				return err
			}
			var exists bool
			if err = transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.users)`).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return ErrAlreadyInitialized
			}
		}
		var imageEnabled bool
		if err = transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.computer_images WHERE id=$1 AND is_enabled)`, request.ImageID).Scan(&imageEnabled); err != nil {
			return err
		}
		if !imageEnabled {
			return ErrImageUnavailable
		}
		var nodeID string
		err = transaction.QueryRowContext(ctx, `SELECT tombot.reserve_computer_node($1,$2)`, request.NodeID, request.ImageID).Scan(&nodeID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoCapacity
		}
		if err != nil {
			return err
		}
		operation = Operation{ID: ids.New(), UserID: ids.New(), ComputerID: ids.New(), Status: "pending", Stage: "reserved", Attempt: 1}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.users(id,username,display_name,role,status) VALUES($1,$2,$3,$4,'provisioning')`, operation.UserID, request.Username, request.DisplayName, request.Role); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.computers(user_id,id,node_id,image_id,status,volume_ref,cpu_millis,memory_bytes,disk_bytes,process_limit)
			VALUES($1,$2,$3,$4,'pending',$5,$6,$7,$8,$9)`, operation.UserID, operation.ComputerID, request.NodeID, request.ImageID, "tombot-user-"+operation.UserID, request.Resources.CPUMillis, request.Resources.MemoryBytes, request.Resources.DiskBytes, request.Resources.ProcessLimit); err != nil {
			return err
		}
		err = transaction.QueryRowContext(ctx, `INSERT INTO tombot.user_creation_operations(id,user_id,computer_id,actor_id,idempotency_key,request_digest,status,stage,deadline_at,request_id)
			VALUES($1,$2,$3,nullif($4,''),$5,$6,'pending','reserved',transaction_timestamp()+$7::bigint*interval '1 millisecond',$8) RETURNING created_at,updated_at`, operation.ID, operation.UserID, operation.ComputerID, actorID, request.IdempotencyKey, digest[:], service.options.OperationTimeout.Milliseconds(), request.RequestID).Scan(&operation.CreatedAt, &operation.UpdatedAt)
		if err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.user_limits(user_id,concurrent_model_calls,concurrent_desktops,queued_runs) VALUES($1,$2,$3,$4)`, operation.UserID, service.options.ModelConcurrency, service.options.DesktopConcurrency, service.options.QueuedRuns); err != nil {
			return err
		}
		return audit(ctx, transaction, actorID, operation.UserID, "受理用户创建", request.RequestID, "succeeded", map[string]any{"operation_id": operation.ID, "role": request.Role, "node_id": request.NodeID})
	})
	if err != nil {
		return Operation{}, err
	}
	return operation, nil
}

func audit(ctx context.Context, transaction *sql.Tx, actorID, userID, action, requestID, result string, changes map[string]any) error {
	encoded, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	_, err = transaction.ExecContext(ctx, `INSERT INTO tombot.admin_audit_events(id,actor_id,action,target_kind,target_id,request_id,result,changes)
		VALUES($1,nullif($2,''),$3,'user',$4,$5,$6,$7)`, ids.New(), actorID, action, userID, requestID, result, string(encoded))
	return err
}
