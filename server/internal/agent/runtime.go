package agent

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/orchestrator"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/secret"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/store"
	"io.yueyang/tom/internal/usage"
	"io.yueyang/tom/internal/workspace"
)

type Limits struct {
	Concurrent  int
	MaxQueued   int
	MaxSteps    int
	WallTime    time.Duration
	StepTime    time.Duration
	BudgetMicro int64
}

func DefaultLimits() Limits {
	return Limits{Concurrent: 3, MaxQueued: 1024, MaxSteps: 60, WallTime: 30 * time.Minute, StepTime: 180 * time.Second, BudgetMicro: 2_000_000}
}

// Tools 将工具注册、策略与实际执行留在工具领域；执行器必须遵守取消并返回唯一结果。
type Tools interface {
	Schemas(domain.Bot) []provider.ToolSchema
	Execute(context.Context, domain.ToolInvocation, domain.ToolUseBlock) (domain.ToolResultBlock, error)
}

type Dependencies struct {
	Files    *workspace.Files
	DB       *sql.DB
	Events   *bus.Bus
	Catalog  *service.Catalog
	Bots     *service.Bots
	Memories *service.Memories
	Secrets  *secret.Manager
	Factory  service.ProviderFactory
	Tools    Tools
	Logger   *slog.Logger
}

type job struct {
	run             domain.Run
	bot             domain.Bot
	model           domain.Model
	spec            domain.Provider
	ctx             context.Context
	cancel          context.CancelCauseFunc
	start           chan struct{}
	started         bool
	summaryDone     chan error
	group           *orchestrator.Task
	brief           string
	preflightErr    error
	completed       chan jobOutcome
	done            chan struct{}
	outcome         jobOutcome
	executionCtx    context.Context
	clock           *executionClock
	waitingUser     bool
	waitingChildren bool
	humanWaits      int
	humanResume     func()
	holdsSlot       bool
	resuming        bool
	resume          chan struct{}
	children        []*job
	replyRequired   bool
	deliverToUser   bool
	dm              bool
	peerBotID       string
}

// Runtime 只接受服务根上下文；HTTP 断开不会取消执行。队列与 Bot 占用由一把锁维护。
type Runtime struct {
	deps          Dependencies
	limits        Limits
	root          context.Context
	mu            sync.Mutex
	jobs          map[string]*job
	introductions map[string]*introductionCall
	groups        map[string]*groupExecution
	queue         []*job
	busy          map[string]bool
	active        int
	closed        bool
	wg            sync.WaitGroup
}

func New(ctx context.Context, deps Dependencies, limits Limits) (*Runtime, error) {
	if deps.Files == nil || deps.DB == nil || deps.Events == nil || deps.Catalog == nil || deps.Bots == nil || deps.Memories == nil || deps.Secrets == nil || deps.Factory == nil || deps.Logger == nil {
		return nil, errors.New("运行时依赖不完整")
	}
	if limits.Concurrent < 1 || limits.MaxQueued < 1 || limits.MaxSteps < 1 || limits.WallTime <= 0 || limits.StepTime <= 0 || limits.BudgetMicro < 1 {
		return nil, errors.New("运行时上限无效")
	}
	r := &Runtime{deps: deps, limits: limits, root: ctx, jobs: map[string]*job{}, introductions: map[string]*introductionCall{}, groups: map[string]*groupExecution{}, busy: map[string]bool{}}
	if err := r.recover(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

type SendInput struct {
	isWelcome bool

	Attachments    []domain.AttachmentInput `json:"attachments,omitempty"`
	Content        string                   `json:"content"`
	Mentions       []string                 `json:"mentions,omitempty"`
	ReplyMode      string                   `json:"reply_mode,omitempty"`
	ReplyToID      string                   `json:"reply_to_id,omitempty"`
	RecipientBotID string                   `json:"recipient_bot_id,omitempty"`
}
type Submission struct {
	TurnID    string `json:"turn_id"`
	MessageID string `json:"message_id"`
	RunID     string `json:"run_id,omitempty"`
}

// Submit 在一个事务内登记用户消息、回合与队列事件；任何冲突都不会留下半条消息。
func (r *Runtime) Submit(ctx context.Context, conversationID string, input SendInput, actor string) (Submission, error) {
	return r.submit(ctx, conversationID, input, actor, "")
}

// Welcome 仅对尚无消息的会话生成开场白；触发消息为系统上下文，不冒充用户发言。
func (r *Runtime) Welcome(ctx context.Context, conversationID string) (Submission, error) {
	return r.submit(ctx, conversationID, SendInput{isWelcome: true, Content: "You have just been created by the user. Greet them in Chinese in your own voice, briefly introduce how you can help based on your identity and role, and invite their first request. Keep it warm and concise. Do not use tools or claim any work has already been done."}, "desktop", "")
}

func (r *Runtime) submit(ctx context.Context, conversationID string, input SendInput, actor, routineRunID string) (Submission, error) {
	var submission Submission
	input.Content = strings.TrimSpace(input.Content)
	if input.Content == "" && len(input.Attachments) == 0 || len(input.Content) > 64<<10 {
		return submission, domain.NewError(400, "INVALID_INPUT", "Provide text or attachments. Text is limited to 65536 bytes.")
	}
	blocks, err := r.userBlocks(ctx, conversationID, input)
	if err != nil {
		return submission, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.root.Err() != nil {
		return submission, domain.NewError(503, "RUNTIME_STOPPING", "The runtime is stopping.")
	}
	if len(r.jobs)+len(r.groups) >= r.limits.MaxQueued+r.limits.Concurrent {
		return submission, domain.NewError(429, "RUN_QUEUE_FULL", "The run queue is full.")
	}
	var botID, initiatorID, kind string
	var archived bool
	if err := r.deps.DB.QueryRowContext(ctx, "SELECT kind,COALESCE(bot_id,''),COALESCE(initiator_bot_id,''),is_archived FROM conversations WHERE id=?", conversationID).Scan(&kind, &botID, &initiatorID, &archived); err != nil {
		return submission, notFound(err)
	}
	if kind == "group" && !archived {
		if input.RecipientBotID != "" {
			return submission, domain.NewError(400, "INVALID_INPUT", "Use mentions to select group recipients.")
		}
		return r.submitGroupLocked(ctx, conversationID, input, blocks, actor, routineRunID)
	}
	if len(input.Mentions) > 0 || (input.ReplyMode != "" && input.ReplyMode != "single") {
		return submission, domain.NewError(400, "INVALID_INPUT", "Mentions and group reply mode require a group conversation.")
	}
	if (kind != "direct" && kind != "bot_dm") || archived {
		return submission, domain.NewError(400, "CONVERSATION_UNAVAILABLE", "Use an active conversation.")
	}
	peerID := initiatorID
	if input.RecipientBotID != "" {
		if kind != "bot_dm" || (input.RecipientBotID != botID && input.RecipientBotID != initiatorID) {
			return submission, domain.NewError(400, "INVALID_DM_RECIPIENT", "The recipient must be a participant in this bot conversation.")
		}
		if input.RecipientBotID == initiatorID {
			peerID = botID
		}
		botID = input.RecipientBotID
	}
	bot, err := r.deps.Bots.Get(ctx, botID)
	if err != nil {
		return submission, err
	}
	if bot.ModelID == nil {
		return submission, domain.NewError(400, "MODEL_NOT_CONFIGURED", "Select a model for this bot before sending a message.")
	}
	model, err := r.deps.Catalog.Model(ctx, *bot.ModelID)
	if err != nil {
		return submission, err
	}
	spec, err := r.deps.Catalog.Provider(ctx, model.ProviderID)
	if err != nil {
		return submission, err
	}
	if !spec.IsEnabled {
		return submission, domain.NewError(400, "PROVIDER_DISABLED", "Enable this model provider before sending a message.")
	}
	now := time.Now().UTC()
	submission = Submission{TurnID: ids.New(), MessageID: ids.New(), RunID: ids.New()}
	budget := r.limits.BudgetMicro
	run := domain.Run{ID: submission.RunID, TurnID: submission.TurnID, ConversationID: conversationID, BotID: botID, ModelID: model.ID, Status: "queued", Source: "chat", PlanRole: "solo", BudgetMicro: &budget, CreatedAt: now}
	err = store.Transact(ctx, r.deps.DB, func(tx *sql.Tx) error {
		if err := usage.CheckMonthlyAdmission(ctx, tx, now); err != nil {
			return err
		}
		if routineRunID != "" {
			if err := service.CheckRoutineDispatch(ctx, tx, routineRunID, conversationID); err != nil {
				return err
			}
		}
		if input.isWelcome {
			var count int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages WHERE conversation_id=?", conversationID).Scan(&count); err != nil {
				return err
			}
			if count > 0 {
				return domain.NewError(409, "WELCOME_EXISTS", "会话已经开始，不会重复生成欢迎消息。")
			}
		}
		var active int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM turns WHERE conversation_id=? AND status IN('queued','running','waiting_approval')", conversationID).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return domain.NewError(409, "TURN_IN_PROGRESS", "Wait for or cancel the active turn.")
		}
		// 配置可能在前面的读取之后被修改，接受的任务必须使用仍有效的身份和模型。
		var currentModel string
		var unavailable bool
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(b.model_id,''),(b.is_archived OR c.is_archived) FROM bots b JOIN conversations c ON (c.bot_id=b.id OR c.kind='bot_dm' AND c.initiator_bot_id=b.id) WHERE c.id=? AND b.id=?`, conversationID, botID).Scan(&currentModel, &unavailable); err != nil {
			return notFound(err)
		}
		if unavailable || currentModel != model.ID {
			return domain.NewError(409, "CONFIGURATION_CHANGED", "The bot configuration changed. Reload before sending.")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO turns(id,conversation_id,seq,source,status,created_at) SELECT ?,?,COALESCE(MAX(seq),0)+1,'user','queued',? FROM turns WHERE conversation_id=?`, run.TurnID, conversationID, now.UnixMilli(), conversationID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO runs(id,turn_id,conversation_id,bot_id,source,status,model_id,prompt,plan_role,budget_micro,created_at) VALUES(?,?,?,?,'chat','queued',?,?,'solo',?,?)`, run.ID, run.TurnID, conversationID, botID, model.ID, input.Content, budget, now.UnixMilli()); err != nil {
			return err
		}
		message := domain.Message{ID: submission.MessageID, ConversationID: conversationID, TurnID: &run.TurnID, SenderKind: "user", Blocks: blocks, Status: "complete", CreatedAt: now}
		if input.isWelcome {
			message.SenderKind = "system"
		}
		if err := service.AppendMessage(ctx, tx, &message); err != nil {
			return err
		}
		if err := service.BindReply(ctx, tx, message.ID, input.ReplyToID, conversationID); err != nil {
			return err
		}
		if _, err := bus.Append(ctx, tx, "run.status", conversationID, map[string]any{"run_id": run.ID, "status": "queued", "turn_id": run.TurnID, "bot_id": botID, "plan_role": "solo", "model_name": model.DisplayName, "queue_position": len(r.queue) + 1}); err != nil {
			return err
		}
		if err := store.Audit(ctx, tx, actor, "turn.create", run.TurnID, map[string]string{"bot_id": botID}); err != nil {
			return err
		}
		if routineRunID != "" {
			if err := service.AttachRoutineTurn(ctx, tx, routineRunID, run.TurnID); err != nil {
				return err
			}
		}
		return appendConversation(ctx, tx, conversationID)
	})
	if err != nil {
		return Submission{}, err
	}
	if routineRunID != "" {
		run.Source = "routine"
	}
	jobCtx, cancel := context.WithCancelCause(r.root)
	j := &job{run: run, bot: bot, model: model, spec: spec, ctx: jobCtx, cancel: cancel, start: make(chan struct{}), done: make(chan struct{}), dm: kind == "bot_dm", peerBotID: peerID}
	r.jobs[run.ID] = j
	r.queue = append(r.queue, j)
	r.wg.Add(1)
	go r.execute(j)
	r.scheduleLocked()
	r.deps.Events.Notify()
	return submission, nil
}

func (r *Runtime) scheduleLocked() {
	if r.closed {
		return
	}
	remaining := r.queue[:0]
	for _, j := range r.queue {
		if j.ctx.Err() == nil && r.active < r.limits.Concurrent && (!r.busy[j.run.BotID] || j.resuming) {
			r.active++
			j.holdsSlot = true
			r.busy[j.run.BotID] = true
			if j.resuming {
				j.resuming = false
				close(j.resume)
			} else {
				j.started = true
				close(j.start)
			}
		} else {
			remaining = append(remaining, j)
		}
	}
	r.queue = remaining
}

func (r *Runtime) execute(j *job) {
	defer r.wg.Done()
	defer func() {
		j.cancel(nil)
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.jobs, j.run.ID)
		if j.holdsSlot {
			r.active--
		}
		if j.started {
			delete(r.busy, j.run.BotID)
		}
		for index, queued := range r.queue {
			if queued == j {
				r.queue = append(r.queue[:index], r.queue[index+1:]...)
				break
			}
		}
		r.scheduleLocked()
	}()
	var err error
	select {
	case <-j.ctx.Done():
		err = j.ctx.Err()
	case <-j.start:
		clock := newExecutionClock(j.ctx, r.limits.WallTime)
		ctx, cancel := context.Context(clock), clock.Close
		r.mu.Lock()
		j.executionCtx = ctx
		j.clock = clock
		r.mu.Unlock()
		err = r.start(ctx, j)
		if err == nil {
			err = j.preflightErr
			if err == nil {
				err = r.loop(ctx, j)
			}
		}
		err = errors.Join(err, r.drainChildren(ctx, j))
		if err == nil && j.deliverToUser {
			err = r.deliverUserResult(ctx, j)
		}
		if err == nil {
			err = ctx.Err()
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = domain.NewError(408, "RUN_TIMEOUT", "The run reached its wall-clock limit.")
		}
		cancel()
	}
	status, code := "succeeded", ""
	if err != nil {
		status, code = "failed", "RUN_FAILED"
		var public *domain.Error
		if errors.As(err, &public) {
			code = public.Code
		}
		if errors.Is(err, context.Canceled) {
			status, code = "canceled", "RUN_CANCELED"
		}
	}
	if r.root.Err() != nil {
		status, code = "interrupted", "RUN_INTERRUPTED"
	}
	// 清理落库独立于已取消的请求上下文，仍有明确期限；失败由启动恢复接手。
	cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	finishErr := r.finish(cleanup, j.run, status, code)
	if finishErr != nil {
		r.deps.Logger.Error("执行收尾持久化失败", "run_id", j.run.ID, "error_code", code)
		err = errors.Join(err, finishErr)
	} else if err != nil {
		r.deps.Logger.Warn("执行停止", "run_id", j.run.ID, "bot_id", j.run.BotID, "error_code", code)
	}
	j.outcome = jobOutcome{runID: j.run.ID, status: status, code: code, err: err, persistErr: finishErr}
	close(j.done)
	if j.completed != nil {
		j.completed <- j.outcome
	}
}

// Cancel 幂等取消指定回合下所有排队和运行中的任务，最终状态通过事件确认。
func (r *Runtime) Cancel(ctx context.Context, turnID, actor string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := store.Transact(ctx, r.deps.DB, func(tx *sql.Tx) error {
		var id string
		if err := tx.QueryRowContext(ctx, "SELECT id FROM turns WHERE id=?", turnID).Scan(&id); err != nil {
			return notFound(err)
		}
		return store.Audit(ctx, tx, actor, "turn.cancel", turnID, map[string]string{})
	}); err != nil {
		return err
	}
	for _, j := range r.jobs {
		if j.run.TurnID == turnID {
			j.cancel(context.Canceled)
		}
	}
	if group := r.groups[turnID]; group != nil {
		group.cancel()
	}
	return nil
}

// Close 拒绝新任务并等待所有上游与工具释放资源，必须在关闭数据库之前执行。
func (r *Runtime) Close() {
	r.mu.Lock()
	r.closed = true
	for _, group := range r.groups {
		group.cancel()
	}
	for _, j := range r.jobs {
		j.cancel(context.Canceled)
	}
	for _, call := range r.introductions {
		call.cancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
}
func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return domain.NewError(404, "NOT_FOUND", "The execution resource does not exist.")
	}
	return err
}
