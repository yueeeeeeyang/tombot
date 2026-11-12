package modelusage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/store/postgres"
)

// Reference 来自已校验租约及操作记录；不得由用户接口接收。
type Reference struct {
	UserID       string
	OperationID  string
	RunID        string
	FencingToken int64
	Attempt      int
}

func validID(value string) bool { _, err := ulid.ParseStrict(value); return err == nil }
func (reference Reference) valid() bool {
	return validID(reference.UserID) && validID(reference.OperationID) && validID(reference.RunID) && reference.FencingToken > 0 && reference.Attempt > 0
}

type reservation struct {
	Reference
	Period         string
	State          string
	Reserved, Held int64
	IncludesCache  bool
	Prices         modelregistry.Prices
	Actual, Cost   *int64
	Expired        bool
	NeedsReconcile bool
}

func lockAccount(ctx context.Context, transaction *sql.Tx, userID string) error {
	if _, err := transaction.ExecContext(ctx, `INSERT INTO tombot.usage_accounts(user_id) VALUES($1) ON CONFLICT(user_id) DO NOTHING`, userID); err != nil {
		return err
	}
	var id string
	return transaction.QueryRowContext(ctx, `SELECT user_id FROM tombot.usage_accounts WHERE user_id=$1 FOR UPDATE`, userID).Scan(&id)
}
func readReservation(ctx context.Context, transaction *sql.Tx, userID, id string) (reservation, error) {
	var result reservation
	var prices []byte
	err := transaction.QueryRowContext(ctx, `SELECT user_id,id,run_id,fencing_token,attempt,to_char(period,'YYYY-MM-DD'),state,reserved_tokens,held_tokens,input_includes_cache,prices,actual_tokens,cost_micro_usd,slot_expires_at<=clock_timestamp(),needs_reconcile
		FROM tombot.usage_reservations WHERE user_id=$1 AND id=$2 FOR UPDATE`, userID, id).Scan(&result.UserID, &result.OperationID, &result.RunID, &result.FencingToken, &result.Attempt, &result.Period, &result.State, &result.Reserved, &result.Held, &result.IncludesCache, &prices, &result.Actual, &result.Cost, &result.Expired, &result.NeedsReconcile)
	if errors.Is(err, sql.ErrNoRows) {
		return reservation{}, ErrNotFound
	}
	if err != nil {
		return reservation{}, err
	}
	if json.Unmarshal(prices, &result.Prices) != nil {
		return reservation{}, ErrInvalidInput
	}
	return result, nil
}

// Reserve 必须与操作插入处于同一事务，并由调用者持有有效运行租约。
// 配置只能来自该运行固定版本的 modelregistry.Use；预留上限不作为实际用量展示。
func Reserve(ctx context.Context, transaction *sql.Tx, reference Reference, configuration modelregistry.Configuration, lifetime time.Duration) error {
	if transaction == nil || !reference.valid() || configuration.Validate() != nil || lifetime < time.Second || lifetime > 3*time.Minute {
		return ErrInvalidInput
	}
	if err := lockAccount(ctx, transaction, reference.UserID); err != nil {
		return err
	}
	var botID, policyID string
	var version int64
	err := transaction.QueryRowContext(ctx, `SELECT r.bot_id,r.model_policy_id,r.model_version FROM tombot.runs r JOIN tombot.run_operations o ON o.user_id=r.user_id AND o.run_id=r.id
		WHERE r.user_id=$1 AND r.id=$2 AND o.id=$3 AND o.fencing_token=$4 AND o.attempt=$5 AND o.kind='model' AND o.status='started' AND r.status='running'`, reference.UserID, reference.RunID, reference.OperationID, reference.FencingToken, reference.Attempt).Scan(&botID, &policyID, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var maximum, active int
	var limit *int64
	if err = transaction.QueryRowContext(ctx, `SELECT concurrent_model_calls,monthly_token_limit,(SELECT count(*) FROM tombot.usage_reservations WHERE user_id=$1 AND NOT slot_released) FROM tombot.user_limits WHERE user_id=$1`, reference.UserID).Scan(&maximum, &limit, &active); err != nil {
		return err
	}
	if active >= maximum {
		return ErrConcurrency
	}
	var period string
	if err = transaction.QueryRowContext(ctx, `SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-01')`).Scan(&period); err != nil {
		return err
	}
	if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.usage_periods(user_id,period) VALUES($1,$2::date) ON CONFLICT(user_id,period) DO NOTHING`, reference.UserID, period); err != nil {
		return err
	}
	var known, held int64
	if err = transaction.QueryRowContext(ctx, `SELECT known_tokens,held_tokens FROM tombot.usage_periods WHERE user_id=$1 AND period=$2::date`, reference.UserID, period).Scan(&known, &held); err != nil {
		return err
	}
	reserved := int64(configuration.ContextLimit) + int64(configuration.OutputLimit)
	if held > math.MaxInt64-known || reserved > math.MaxInt64-known-held || limit != nil && (known+held > *limit || reserved > *limit-known-held) {
		return ErrQuota
	}
	prices, err := json.Marshal(configuration.Prices)
	if err != nil {
		return err
	}
	if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.usage_reservations(user_id,id,run_id,bot_id,policy_id,model_version,period,fencing_token,attempt,reserved_tokens,held_tokens,prices,input_includes_cache,state,slot_expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7::date,$8,$9,$10,$10,$11,$12,'active',clock_timestamp()+$13::bigint*interval '1 millisecond')`, reference.UserID, reference.OperationID, reference.RunID, botID, policyID, version, period, reference.FencingToken, reference.Attempt, reserved, prices, configuration.Protocol == "openai_compat", lifetime.Milliseconds()); err != nil {
		return err
	}
	_, err = transaction.ExecContext(ctx, `UPDATE tombot.usage_periods SET held_tokens=held_tokens+$3 WHERE user_id=$1 AND period=$2::date`, reference.UserID, period, reserved)
	return err
}

// AppendEvidence 校验原操作身份后只追加不可变计量证据；过期 Worker 不因此取得运行写权限。
// 同一个证据 ID 重试必须保持内容一致；每次调用最多保留 16 份证据。
func AppendEvidence(ctx context.Context, transaction *sql.Tx, reference Reference, evidenceID string, observation Observation) error {
	if transaction == nil || !reference.valid() || !validID(evidenceID) {
		return ErrInvalidInput
	}
	if err := lockAccount(ctx, transaction, reference.UserID); err != nil {
		return err
	}
	stored, err := readReservation(ctx, transaction, reference.UserID, reference.OperationID)
	if err != nil {
		return err
	}
	if stored.Reference != reference {
		return ErrConflict
	}
	return appendEvidence(ctx, transaction, stored, evidenceID, "worker", observation)
}

func appendEvidence(ctx context.Context, transaction *sql.Tx, stored reservation, evidenceID, source string, observation Observation) error {
	measurement, err := Measure(observation.Usage, stored.Prices, stored.IncludesCache)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(append([]byte(source+":"), encoded...))
	var old []byte
	var reservationID string
	err = transaction.QueryRowContext(ctx, `SELECT reservation_id,evidence_digest FROM tombot.model_usage WHERE user_id=$1 AND id=$2`, stored.UserID, evidenceID).Scan(&reservationID, &old)
	if err == nil {
		if reservationID != stored.OperationID || !bytes.Equal(old, digest[:]) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if stored.State == "settled" && observation.Final && measurement.Tokens != nil {
		if *measurement.Tokens != *stored.Actual || measurement.CostMicroUSD != nil && stored.Cost != nil && *measurement.CostMicroUSD != *stored.Cost {
			return ErrConflict
		}
	}
	if stored.Actual != nil && measurement.LowerBound > *stored.Actual {
		return ErrConflict
	}
	if observation.Final && measurement.Tokens != nil {
		var conflict bool
		if err = transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.model_usage WHERE user_id=$1 AND reservation_id=$2
			AND (lower_bound_tokens>$3::bigint OR is_final AND (measured_tokens<>$3::bigint OR cost_micro_usd IS NOT NULL AND $4::bigint IS NOT NULL AND cost_micro_usd<>$4::bigint
			OR (reported->>'input_tokens')::bigint<>$5::bigint OR (reported->>'output_tokens')::bigint<>$6::bigint
			OR (reported->>'cache_read_tokens')::bigint<>$7::bigint OR (reported->>'cache_write_tokens')::bigint<>$8::bigint)))`, stored.UserID, stored.OperationID, measurement.Tokens, measurement.CostMicroUSD, observation.Usage.InputTokens, observation.Usage.OutputTokens, observation.Usage.CacheReadTokens, observation.Usage.CacheWriteTokens).Scan(&conflict); err != nil {
			return err
		}
		if conflict {
			return ErrConflict
		}
	}
	var count int
	if err = transaction.QueryRowContext(ctx, `SELECT count(*) FROM tombot.model_usage WHERE user_id=$1 AND reservation_id=$2`, stored.UserID, stored.OperationID).Scan(&count); err != nil {
		return err
	}
	if count >= 16 {
		return ErrConflict
	}
	var reported any
	if observation.Usage != nil {
		content, err := json.Marshal(observation.Usage)
		if err != nil {
			return err
		}
		reported = content
	}
	if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.model_usage(user_id,id,reservation_id,source,reported,is_final,measured_tokens,lower_bound_tokens,cost_micro_usd,evidence_digest)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, stored.UserID, evidenceID, stored.OperationID, source, reported, observation.Final, measurement.Tokens, measurement.LowerBound, measurement.CostMicroUSD, digest[:]); err != nil {
		return err
	}
	_, err = transaction.ExecContext(ctx, `UPDATE tombot.usage_reservations SET needs_reconcile=true,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, stored.UserID, stored.OperationID)
	return err
}

// Settle 由有效 Worker 在同一事务写入结果、计量与运行操作；失败会共同回滚。
func Settle(ctx context.Context, transaction *sql.Tx, reference Reference, evidenceID string, observation Observation) error {
	if err := AppendEvidence(ctx, transaction, reference, evidenceID, observation); err != nil {
		return err
	}
	stored, err := readReservation(ctx, transaction, reference.UserID, reference.OperationID)
	if err != nil {
		return err
	}
	return applyEvidence(ctx, transaction, stored)
}

func applyEvidence(ctx context.Context, transaction *sql.Tx, stored reservation) error {
	if !stored.NeedsReconcile && stored.State != "active" {
		return nil
	}
	var actual, cost *int64
	var lowerBound int64
	var final bool
	err := transaction.QueryRowContext(ctx, `SELECT is_final,measured_tokens,cost_micro_usd,lower_bound_tokens FROM tombot.model_usage WHERE user_id=$1 AND reservation_id=$2
		ORDER BY (is_final AND measured_tokens IS NOT NULL) DESC,(cost_micro_usd IS NOT NULL) DESC,created_at DESC,id DESC LIMIT 1`, stored.UserID, stored.OperationID).Scan(&final, &actual, &cost, &lowerBound)
	if err != nil {
		return err
	}
	if err = transaction.QueryRowContext(ctx, `SELECT COALESCE(max(lower_bound_tokens),0) FROM tombot.model_usage WHERE user_id=$1 AND reservation_id=$2`, stored.UserID, stored.OperationID).Scan(&lowerBound); err != nil {
		return err
	}
	if stored.State == "settled" {
		if !final || actual == nil || *actual != *stored.Actual {
			return ErrConflict
		}
		_, err = transaction.ExecContext(ctx, `UPDATE tombot.usage_reservations SET cost_micro_usd=COALESCE(cost_micro_usd,$3::bigint),needs_reconcile=false,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, stored.UserID, stored.OperationID, cost)
		if err != nil {
			return err
		}
		if stored.Cost == nil && cost != nil {
			return usageEvent(ctx, transaction, stored, "settled", actual, cost)
		}
		return nil
	}
	state, newHeld, known := "unknown", max(stored.Held, lowerBound), int64(0)
	if final && actual != nil {
		state, newHeld, known = "settled", 0, *actual
	} else {
		actual, cost = nil, nil
	}
	result, err := transaction.ExecContext(ctx, `UPDATE tombot.usage_periods SET held_tokens=held_tokens-$3+$4,known_tokens=known_tokens+$5
		WHERE user_id=$1 AND period=$2::date AND held_tokens>=$3 AND held_tokens-$3<=$6::bigint-$4::bigint AND known_tokens<=$6::bigint-$5::bigint`, stored.UserID, stored.Period, stored.Held, newHeld, known, int64(math.MaxInt64))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrConflict
	}
	if _, err = transaction.ExecContext(ctx, `UPDATE tombot.usage_reservations SET held_tokens=$3,state=$4,slot_released=true,needs_reconcile=false,actual_tokens=$5,cost_micro_usd=$6,updated_at=clock_timestamp()
		WHERE user_id=$1 AND id=$2`, stored.UserID, stored.OperationID, newHeld, state, actual, cost); err != nil {
		return err
	}
	return usageEvent(ctx, transaction, stored, state, actual, cost)
}

func usageEvent(ctx context.Context, transaction *sql.Tx, stored reservation, state string, actual, cost *int64) error {
	payload, err := json.Marshal(map[string]any{"run_id": stored.RunID, "state": state, "actual_tokens": actual, "cost_micro_usd": cost})
	if err != nil {
		return err
	}
	_, err = postgres.AppendEvent(ctx, transaction, stored.UserID, "model_usage.updated", stored.OperationID, payload)
	return err
}
