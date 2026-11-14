package modelregistry

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"
	"slices"
	"sort"
)

func acceptsCandidate(state candidateState, requirements Requirements) bool {
	c := state.candidate.Configuration
	return state.available && (!requirements.Tools || c.SupportsTools) && (!requirements.Vision || c.SupportsVision) && requirements.InputTokens <= c.ContextLimit-c.OutputLimit
}

// Route 按运行串行选择候选并持久保存；重复调用不再次消耗轮询位置，故障切换不会重选已尝试模型。
// failedModel 只能来自执行器确认的可切换失败；数据库外键核对运行与策略绑定。
func (service *Service) Route(ctx context.Context, selection Selection, requirements Requirements, failedModel string) (Selection, error) {
	if !validID(selection.UserID) || !validID(selection.BotID) || !validID(selection.RunID) || !validID(selection.PolicyID) || selection.Version < 1 || requirements.InputTokens < 0 || requirements.InputTokens > 100_000_000 || failedModel != "" && !validID(failedModel) {
		return Selection{}, ErrInvalidInput
	}
	if _, err := service.tenantPolicy(ctx, selection.UserID, selection.BotID, true); err != nil {
		return Selection{}, err
	}
	// 租户侧核对运行身份，控制连接只处理模型元数据，不读取对话正文。
	if err := service.tenant.Snapshot(ctx, selection.UserID, func(tx *sql.Tx) error {
		return requireRow(tx.QueryRowContext(ctx, `SELECT id FROM tombot.runs WHERE user_id=$1 AND id=$2 AND bot_id=$3 AND model_policy_id=$4 AND model_version=$5 AND status='running'`, selection.UserID, selection.RunID, selection.BotID, selection.PolicyID, selection.Version))
	}); err != nil {
		return Selection{}, err
	}
	err := service.transaction(ctx, func(tx *sql.Tx) error {
		// 运行级事务锁同时覆盖首次分配和失败切换，跨进程重入只返回已提交结果。
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, selection.RunID); err != nil {
			return err
		}
		state, err := readVersion(ctx, tx, selection.PolicyID, selection.Version, true)
		if err != nil {
			return err
		}
		if !state.isAvailable || state.version.VerifiedAt == nil {
			return ErrUnavailable
		}
		var current string
		var encoded []byte
		var attempted []string
		err = tx.QueryRowContext(ctx, `SELECT model_id,attempted_models FROM tombot.model_routes WHERE user_id=$1 AND run_id=$2 AND policy_id=$3 AND version=$4 FOR UPDATE`, selection.UserID, selection.RunID, selection.PolicyID, selection.Version).Scan(&current, &encoded)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		exists := err == nil
		if exists {
			if json.Unmarshal(encoded, &attempted) != nil {
				return ErrUnavailable
			}
			if failedModel != "" && failedModel != current {
				failedModel = ""
			}
			if failedModel == "" {
				for _, c := range state.candidates {
					if c.candidate.ModelID == current && acceptsCandidate(c, requirements) {
						selection.ModelID = current
						return nil
					}
				}
			}
		} else if failedModel != "" {
			return ErrConflict
		}
		eligible := []Candidate{}
		for _, c := range state.candidates {
			if !slices.Contains(attempted, c.candidate.ModelID) && acceptsCandidate(c, requirements) {
				eligible = append(eligible, c.candidate)
			}
		}
		if len(eligible) == 0 {
			return ErrNoCandidate
		}
		if exists || state.version.RoutingMode != "round_robin" {
			sort.SliceStable(eligible, func(i, j int) bool { return eligible[i].Priority < eligible[j].Priority })
		}
		chosen := eligible[0]
		if !exists {
			switch state.version.RoutingMode {
			case "weighted":
				total := 0
				for _, c := range eligible {
					total += c.Weight
				}
				ticket, err := rand.Int(rand.Reader, big.NewInt(int64(total)))
				if err != nil {
					return errors.New("生成模型路由随机数失败")
				}
				chosen = weightedCandidate(eligible, int(ticket.Int64()))
			case "round_robin":
				var ticket int64
				if err := tx.QueryRowContext(ctx, `INSERT INTO tombot.model_route_counters(policy_id,version,next_ticket) VALUES($1,$2,1)
     ON CONFLICT(policy_id,version) DO UPDATE SET next_ticket=CASE WHEN model_route_counters.next_ticket=9223372036854775807 THEN 1 ELSE model_route_counters.next_ticket+1 END RETURNING next_ticket-1`, selection.PolicyID, selection.Version).Scan(&ticket); err != nil {
					return err
				}
				chosen = eligible[ticket%int64(len(eligible))]
			}
		}
		selection.ModelID = chosen.ModelID
		attempted = append(attempted, chosen.ModelID)
		encoded, err = json.Marshal(attempted)
		if err != nil {
			return err
		}
		if exists {
			_, err = tx.ExecContext(ctx, `UPDATE tombot.model_routes SET model_id=$3,attempted_models=$4,updated_at=clock_timestamp() WHERE user_id=$1 AND run_id=$2`, selection.UserID, selection.RunID, chosen.ModelID, encoded)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO tombot.model_routes(user_id,run_id,policy_id,version,model_id,attempted_models) VALUES($1,$2,$3,$4,$5,$6)`, selection.UserID, selection.RunID, selection.PolicyID, selection.Version, chosen.ModelID, encoded)
		}
		return err
	})
	if err != nil {
		return Selection{}, err
	}
	return selection, nil
}

func weightedCandidate(candidates []Candidate, ticket int) Candidate {
	for _, c := range candidates {
		if ticket < c.Weight {
			return c
		}
		ticket -= c.Weight
	}
	panic("模型路由权重票号越界")
}
