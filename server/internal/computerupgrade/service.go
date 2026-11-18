// Package computerupgrade 负责已投用电脑的前向迁移及控制端就绪提交。
package computerupgrade

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computercontrol"
	"io.yueyang/tom/internal/store/postgres"
)

const (
	scanInterval = 5 * time.Second
	drainTimeout = 30 * time.Minute
	maxAttempts  = 5
)

var errLeaseLost = errors.New("电脑镜像升级租约已失效")

// Service 在受限工作循环中逐台迁移电脑。数据库锁只保护领取和提交，节点调用在事务外。
type Service struct {
	database *postgres.Database
	node     computer.ImageUpgrader
	sessions *computercontrol.Service
	timeout  time.Duration
	logger   *slog.Logger
}

// New 组合控制库、原会话清理器和节点镜像升级入口；调用预算沿用节点请求配置。
func New(database *postgres.Database, node computer.ImageUpgrader, sessions *computercontrol.Service, timeout time.Duration, logger *slog.Logger) *Service {
	return &Service{database: database, node: node, sessions: sessions, timeout: timeout, logger: logger}
}

type migration struct {
	upgrade                  computer.ImageUpgrade
	sourceImage, targetImage string
	state                    string
	lease                    time.Time
	attempt                  int
}

// Run 持续对账，暂时故障留在持久计划中。一次只处理一台电脑，避免压满节点资源。
func (service *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		progress, err := service.process(ctx)
		if err != nil && ctx.Err() == nil && !errors.Is(err, errLeaseLost) {
			service.logger.WarnContext(ctx, "已有电脑镜像升级对账未完成")
		}
		if progress && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return ctx.Err()
}

func (service *Service) process(ctx context.Context) (bool, error) {
	plan, err := service.claim(ctx)
	if err != nil || plan == nil {
		return false, err
	}
	if plan.state == "draining" {
		return true, service.drain(ctx, *plan)
	}
	call, cancel := context.WithTimeout(ctx, service.timeout)
	observation, err := service.node.UpgradeComputer(call, plan.upgrade)
	cancel()
	commit, stop := context.WithTimeout(context.WithoutCancel(ctx), service.timeout)
	defer stop()
	if err != nil || !validObservation(plan.upgrade, observation) {
		code := "computer_upgrade_receipt_unconfirmed"
		var failure *computer.Failure
		if errors.As(err, &failure) {
			code = failure.Code
		} else if errors.Is(err, context.DeadlineExceeded) {
			code = "node_call_timeout"
		}
		service.logger.WarnContext(ctx, "已有电脑目标实例尚未通过验收", "node_id", plan.upgrade.Target.NodeID, "computer_id", plan.upgrade.Target.ComputerID, "image_id", plan.targetImage, "attempt", plan.attempt, "error_code", code)
		return true, service.retry(commit, *plan)
	}
	if err = service.finish(commit, *plan, observation); err != nil {
		return true, err
	}
	service.logger.InfoContext(ctx, "已有电脑已切换到验收通过的新镜像", "node_id", plan.upgrade.Target.NodeID, "computer_id", plan.upgrade.Target.ComputerID, "image_id", plan.targetImage, "generation", plan.upgrade.Target.Generation)
	return true, nil
}

func validObservation(upgrade computer.ImageUpgrade, observation computer.Observation) bool {
	return observation.Matches(upgrade.Target) && observation.State == "ready" && observation.Capabilities.Ready() && observation.ContainerID != upgrade.CurrentContainerID
}

func decodeSpecification(encoded []byte) (computer.Specification, error) {
	var specification computer.Specification
	if err := json.Unmarshal(encoded, &specification); err != nil {
		return specification, errors.New("电脑镜像升级规格记录无法解码")
	}
	return specification, specification.Validate()
}

func lockComputer(ctx context.Context, tx *sql.Tx, userID, computerID string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO tombot.computer_execution_heads(user_id,computer_id) VALUES($1,$2) ON CONFLICT (user_id) DO NOTHING`, userID, computerID); err != nil {
		return err
	}
	return tx.QueryRowContext(ctx, `SELECT user_id FROM tombot.computer_execution_heads WHERE user_id=$1 FOR UPDATE`, userID).Scan(&userID)
}
