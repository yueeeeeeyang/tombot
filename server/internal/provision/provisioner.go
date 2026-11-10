package provision

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/store"
)

// Step 先验证、再按需修复、最后重新验证；阻塞步骤失败不得宣告就绪。
type Step struct {
	ID, Title, Phase string
	Blocking         bool
	Run, Verify      func(context.Context) error
}
type StepStatus struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	Detail     string `json:"detail,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}
type State struct {
	Phase string       `json:"phase"`
	Steps []StepStatus `json:"steps"`
	Error string       `json:"error,omitempty"`
}

// Provisioner 串行执行启动或修复；状态副本可安全供 HTTP 并发读取。
type Provisioner struct {
	db      *sql.DB
	bus     *bus.Bus
	steps   []Step
	timeout time.Duration
	mu      sync.RWMutex
	state   State
	runDone chan struct{}
	running bool
}

// New 读取上次诊断；每次启动仍需重新验证实际环境。
func New(ctx context.Context, db *sql.DB, events *bus.Bus, steps []Step, timeout time.Duration) (*Provisioner, error) {
	p := &Provisioner{db: db, bus: events, steps: steps, timeout: timeout, state: State{Phase: "pending", Steps: []StepStatus{}}}
	var previous string
	err := db.QueryRowContext(ctx, "SELECT value_json FROM system_state WHERE key='bootstrap'").Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		if err = json.Unmarshal([]byte(previous), &p.state); err != nil {
			return nil, fmt.Errorf("引导记录损坏: %w", err)
		}
		p.state.Phase = "pending"
	}
	return p, nil
}

func (p *Provisioner) Snapshot() State {
	p.mu.RLock()
	defer p.mu.RUnlock()
	state := p.state
	state.Steps = append([]StepStatus{}, p.state.Steps...)
	return state
}
func (p *Provisioner) Ready() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state.Phase == "ready"
}

// WaitReady 等待已提交的就绪状态。先订阅再检查，避免维护结束时丢失唤醒。
func (p *Provisioner) WaitReady(ctx context.Context) error {
	for {
		wake := p.bus.Wake()
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.Ready() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		}
	}
}

type Status struct {
	State
	Cursor int64 `json:"cursor"`
}

// Status 返回同一时点的引导进度与事件游标。长时间无引导事件时仍可从当前队尾接续。
func (p *Provisioner) Status(ctx context.Context) (Status, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	status := Status{State: p.state}
	status.Steps = append([]StepStatus{}, p.state.Steps...)
	err := p.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(id),0) FROM events").Scan(&status.Cursor)
	return status, err
}

// Claim 原子取得引导执行权；必须在启动异步协程之前调用。
func (p *Provisioner) Claim() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return false
	}
	p.claimLocked()
	return true
}

func (p *Provisioner) claimLocked() {
	p.runDone = make(chan struct{})
	p.running = true
	p.state = State{Phase: "detecting", Steps: []StepStatus{}}
}

// ClaimMaintenance 原子记录修复或重置并取得执行权。清除诊断、审计或事件写入失败时保留原状态。
func (p *Provisioner) ClaimMaintenance(ctx context.Context, actor string, reset bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return domain.NewError(409, "BOOTSTRAP_RUNNING", "Bootstrap is already running.")
	}
	err := store.Transact(ctx, p.db, func(tx *sql.Tx) error {
		action := "bootstrap.repair"
		if reset {
			action = "bootstrap.reset"
			if _, err := tx.ExecContext(ctx, "DELETE FROM capabilities"); err != nil {
				return err
			}
		}
		if err := store.Audit(ctx, tx, actor, action, "bootstrap", map[string]string{}); err != nil {
			return err
		}
		return writeState(ctx, tx, State{Phase: "detecting", Steps: []StepStatus{}})
	})
	if err != nil {
		return err
	}
	p.claimLocked()
	p.bus.Notify()
	return nil
}

// Wait 等待当前引导释放资源；关闭数据库之前必须调用。
func (p *Provisioner) Wait() {
	p.mu.RLock()
	done := p.runDone
	p.mu.RUnlock()
	if done != nil {
		<-done
	}
}

// RunClaimed 执行已领取的引导；diagnose=true 时不安装或修复任何依赖。
func (p *Provisioner) RunClaimed(ctx context.Context, diagnose bool) (result error) {
	defer func() {
		p.mu.Lock()
		p.running = false
		if p.runDone != nil {
			close(p.runDone)
			p.runDone = nil
		}
		if result != nil && p.state.Phase != "failed" {
			p.state.Phase = "failed"
			p.state.Error = "引导进度写入失败，请检查存储后重新体检"
		}
		p.mu.Unlock()
	}()
	state := State{Phase: "detecting", Steps: []StepStatus{}}
	for _, step := range p.steps {
		started := time.Now()
		status := StepStatus{ID: step.ID, Title: step.Title, Status: "running"}
		state.Phase = step.Phase
		state.Steps = append(state.Steps, status)
		if err := p.persist(ctx, state, nil); err != nil {
			return err
		}
		stepCtx, cancel := context.WithTimeout(ctx, p.timeout)
		err := step.Verify(stepCtx)
		if err != nil && !diagnose && step.Run != nil {
			err = step.Run(stepCtx)
			if err == nil {
				err = step.Verify(stepCtx)
			}
		}
		cancel()
		status.DurationMS = time.Since(started).Milliseconds()
		status.Status = "ok"
		if err != nil {
			status.Detail = err.Error()
			status.Status = "degraded"
			if step.Blocking {
				status.Status = "failed"
				state.Phase = "failed"
				state.Error = status.Detail
			}
		}
		state.Steps[len(state.Steps)-1] = status
		if persistErr := p.persist(ctx, state, &status); persistErr != nil {
			return errors.Join(err, persistErr)
		}
		if err != nil && step.Blocking {
			return err
		}
	}
	state.Phase = "ready"
	return p.persist(ctx, state, nil)
}

// writeState 将快照和可续传事件放入同一事务，通知由调用方在提交后发送。
func writeState(ctx context.Context, tx *sql.Tx, state State) error {
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO system_state VALUES('bootstrap',?,?) ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json,updated_at=excluded.updated_at`, string(encoded), time.Now().UnixMilli()); err != nil {
		return err
	}
	_, err = bus.Append(ctx, tx, "bootstrap.step", "", state)
	return err
}

// persist 每一步只更新刚完成的能力。提交成功才向内存和 SSE 发布，避免最终写入失败仍开放业务。
func (p *Provisioner) persist(ctx context.Context, state State, completed *StepStatus) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	err := store.Transact(ctx, p.db, func(tx *sql.Tx) error {
		if completed != nil {
			if _, err := tx.ExecContext(ctx, `INSERT INTO capabilities(key,status,detail,checked_at) VALUES(?,?,?,?) ON CONFLICT(key) DO UPDATE SET status=excluded.status,detail=excluded.detail,checked_at=excluded.checked_at`, completed.ID, completed.Status, completed.Detail, time.Now().UnixMilli()); err != nil {
				return err
			}
		}
		return writeState(ctx, tx, state)
	})
	if err != nil {
		return err
	}
	state.Steps = append([]StepStatus{}, state.Steps...)
	p.state = state
	p.bus.Notify()
	return nil
}

// Diagnose 逐项执行只读验证，保留全部失败，不写持久状态或安装依赖。
func Diagnose(ctx context.Context, steps []Step, timeout time.Duration) (State, error) {
	state := State{Phase: "ready", Steps: []StepStatus{}}
	var failures error
	for _, step := range steps {
		started := time.Now()
		stepCtx, cancel := context.WithTimeout(ctx, timeout)
		err := step.Verify(stepCtx)
		cancel()
		status := StepStatus{ID: step.ID, Title: step.Title, Status: "ok", DurationMS: time.Since(started).Milliseconds()}
		if err != nil {
			status.Detail = err.Error()
			status.Status = "degraded"
			if step.Blocking {
				status.Status = "failed"
				state.Phase = "failed"
				failures = errors.Join(failures, fmt.Errorf("%s：%w", step.Title, err))
			}
		}
		state.Steps = append(state.Steps, status)
	}
	if failures != nil {
		state.Error = failures.Error()
	}
	return state, failures
}
