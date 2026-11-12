package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/bots"
	"log/slog"
	"sync"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computercontrol"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/modelusage"
	"io.yueyang/tom/internal/provider"
)

type ModelDirectory interface {
	Use(context.Context, modelregistry.Selection, func(context.Context, modelregistry.Configuration, []byte) error) error
}
type ModelCaller interface {
	Call(context.Context, modelregistry.Configuration, []byte, modelgateway.Input, func(provider.Event) error) (modelgateway.Result, error)
}

// Tool 描述显式注册的工具；执行位置由注册类型决定，不能由模型参数选择。
type Tool interface {
	Schema() provider.ToolSchema
	UsesComputer() bool
	RequiresVision() bool
}

// ServerTool 只用于服务端业务能力，必须遵守当前授权和调用期限。
type ServerTool interface {
	Tool
	Execute(context.Context, Invocation) (botagent.ToolResult, error)
}

// LocalTool 由容器 Agent 执行，服务端注册对象不具有执行方法。
type LocalTool interface {
	Tool
	RunsInAgent() bool
}

func runsInAgent(tool Tool) bool {
	local, ok := tool.(LocalTool)
	return ok && local.RunsInAgent()
}

type Invocation struct {
	Lease        Lease
	OperationID  string
	Sequence     int64
	Call         botagent.ToolCall
	ApprovalMode string
	Computer     *computer.ExecutionSession
}
type WorkerOptions struct {
	Concurrency                                                                      int
	PollInterval, RunTimeout, ModelTimeout, ToolTimeout, RecordTimeout, AgentTimeout time.Duration
	MaxModelSteps, MaxToolCalls, MaxOutputBytes                                      int
}
type Worker struct {
	executor  *Service
	directory ModelDirectory
	caller    ModelCaller
	tools     map[string]Tool
	schemas   map[string]provider.ToolSchema
	options   WorkerOptions
	logger    *slog.Logger
	computers *computercontrol.Service
	agents    computer.AgentRuntime
}

// NewWorker 组合真实持久执行、受管模型接口和显式工具集合，不注册模拟工具或默认供应商。
func NewWorker(executor *Service, directory ModelDirectory, caller ModelCaller, registered []Tool, computers *computercontrol.Service, agents computer.AgentRuntime, options WorkerOptions, logger *slog.Logger) (*Worker, error) {
	if executor == nil || directory == nil || caller == nil || computers == nil || agents == nil || logger == nil || len(registered) > 128 || options.Concurrency < 1 || options.Concurrency > 64 || options.PollInterval < 100*time.Millisecond || options.PollInterval > 10*time.Second ||
		options.RunTimeout < time.Second || options.RunTimeout > 24*time.Hour || options.ModelTimeout < time.Second || options.ModelTimeout > 2*time.Minute || options.ToolTimeout < time.Second || options.ToolTimeout > 10*time.Minute ||
		options.RecordTimeout < time.Second || options.RecordTimeout > 30*time.Second || options.ModelTimeout > options.RunTimeout || options.ToolTimeout > options.RunTimeout || options.MaxModelSteps < 1 || options.MaxModelSteps > 64 ||
		options.AgentTimeout < 200*time.Millisecond || options.AgentTimeout > 10*time.Second || options.AgentTimeout > options.RunTimeout || options.MaxToolCalls < 1 || options.MaxToolCalls > 16 || options.MaxOutputBytes < 1024 || options.MaxOutputBytes > 24<<10 {
		return nil, ErrInvalidInput
	}
	worker := &Worker{executor: executor, directory: directory, caller: caller, options: options, logger: logger, computers: computers, agents: agents, tools: map[string]Tool{}, schemas: map[string]provider.ToolSchema{}}
	for _, tool := range registered {
		if tool == nil {
			return nil, ErrInvalidInput
		}
		_, server := tool.(ServerTool)
		if local := runsInAgent(tool); local == server || local && !tool.UsesComputer() {
			return nil, ErrInvalidInput
		}
		schema := tool.Schema()
		var object map[string]json.RawMessage
		if !bots.ValidToolName(schema.Name) || worker.tools[schema.Name] != nil || !validText(schema.Description, 4096) || len(schema.InputSchema) > 16<<10 || json.Unmarshal(schema.InputSchema, &object) != nil || object == nil {
			return nil, ErrInvalidInput
		}
		schema.InputSchema = bytes.Clone(schema.InputSchema)
		worker.tools[schema.Name], worker.schemas[schema.Name] = tool, schema
	}
	return worker, nil
}

// Serve 在用户间领取持久任务，并独立对账过期任务和调用预留；停止后保留数据库恢复依据。
func (worker *Worker) Serve(ctx context.Context) error {
	var wait sync.WaitGroup
	wait.Go(func() { worker.reconcile(ctx) })
	for range worker.options.Concurrency {
		wait.Go(func() {
			ownerID := ids.New()
			for ctx.Err() == nil {
				lease, err := worker.executor.Claim(ctx, ownerID)
				if err == nil {
					if _, err = worker.Process(ctx, lease); err != nil && ctx.Err() == nil {
						worker.logger.WarnContext(ctx, "运行未能提交终态，保留恢复记录", "user_id", lease.UserID, "run_id", lease.RunID, "worker_id", ownerID)
					}
				} else if !errors.Is(err, ErrNoWork) && ctx.Err() == nil {
					worker.logger.WarnContext(ctx, "领取持久任务失败", "worker_id", ownerID)
				}
				if !waitPoll(ctx, worker.options.PollInterval) {
					return
				}
			}
		})
	}
	wait.Wait()
	return ctx.Err()
}
func waitPoll(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func (worker *Worker) reconcile(ctx context.Context) {
	for ctx.Err() == nil {
		if _, err := worker.computers.Reconcile(ctx, 100); err != nil && ctx.Err() == nil {
			worker.logger.WarnContext(ctx, "电脑会话清理尚未确认，保持隔离")
		}
		if _, err := worker.executor.Recover(ctx, 100); err != nil && ctx.Err() == nil {
			worker.logger.WarnContext(ctx, "恢复过期运行失败")
		}
		if _, err := worker.executor.ReconcileUsage(ctx, 100); err != nil && ctx.Err() == nil {
			worker.logger.WarnContext(ctx, "对账模型用量失败")
		}
		if _, err := worker.executor.PruneScreenshots(ctx, 100); err != nil && ctx.Err() == nil {
			worker.logger.WarnContext(ctx, "清理过期截图失败")
		}
		if !waitPoll(ctx, worker.options.PollInterval) {
			return
		}
	}
}

// Process 在整个模型与工具循环续约；取消、禁用、归档和租约丢失都会中止后续外部动作。
func (worker *Worker) Process(parent context.Context, lease Lease) (Run, error) {
	deadline, finish := context.WithTimeout(parent, worker.options.RunTimeout)
	defer finish()
	ctx, cancel := context.WithCancelCause(deadline)
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		interval := min(worker.executor.options.LeaseLifetime/3, worker.computers.HeartbeatInterval())
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := worker.executor.Renew(ctx, lease); err != nil {
					cancel(err)
					return
				}
				if err := worker.computers.Renew(ctx, computerReference(lease)); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	defer func() { cancel(context.Canceled); <-renewed }()
	answer, err := worker.execute(ctx, lease)
	if err == nil {
		err = worker.computers.Check(ctx, computerReference(lease))
	}
	cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), worker.options.RecordTimeout)
	cleanupErr := worker.computers.Release(cleanup, computerReference(lease))
	stop()
	if cleanupErr != nil {
		return Run{}, cleanupErr
	}
	if answer != "" && !validText(answer, 64<<10) {
		answer = ""
		if err == nil {
			err = botagent.ErrModelOutput
		}
	}
	if parent.Err() != nil && err != nil {
		return Run{}, parent.Err()
	}
	outcome := Outcome{Status: "succeeded", Answer: answer}
	if err != nil {
		outcome.Status, outcome.ErrorCode = "failed", failureCode(err)
		if errors.Is(err, ErrUncertain) {
			outcome.Status = "unknown"
		}
		if errors.Is(context.Cause(ctx), ErrCanceled) || errors.Is(err, ErrCanceled) {
			outcome.Status, outcome.ErrorCode = "canceled", "CANCELED"
		}
	}
	writeContext, stop := context.WithTimeout(context.WithoutCancel(parent), worker.options.RecordTimeout)
	defer stop()
	run, writeErr := worker.executor.Complete(writeContext, lease, outcome)
	if errors.Is(writeErr, ErrComputerLost) {
		run, writeErr = worker.executor.Complete(writeContext, lease, Outcome{Status: "failed", Answer: answer, ErrorCode: "COMPUTER_LEASE_LOST"})
	}
	if errors.Is(writeErr, ErrCanceled) {
		run, writeErr = worker.executor.Complete(writeContext, lease, Outcome{Status: "canceled", Answer: answer, ErrorCode: "CANCELED"})
	}
	if writeErr != nil {
		return Run{}, writeErr
	}
	worker.logger.InfoContext(writeContext, "运行已结束", "user_id", lease.UserID, "run_id", lease.RunID, "status", run.Status, "error_code", run.ErrorCode)
	return run, nil
}

func failureCode(err error) string {
	switch {
	case errors.Is(err, modelusage.ErrQuota):
		return "MODEL_QUOTA_EXCEEDED"
	case errors.Is(err, modelusage.ErrConcurrency):
		return "MODEL_CONCURRENCY_LIMIT"
	case errors.Is(err, ErrOperationLimit), errors.Is(err, botagent.ErrOperationLimit):
		return "OPERATION_LIMIT"
	case errors.Is(err, ErrContextLimit), errors.Is(err, botagent.ErrContextLimit):
		return "CONTEXT_LIMIT"
	case errors.Is(err, ErrUnavailable):
		return "RESOURCE_UNAVAILABLE"
	case errors.Is(err, computercontrol.ErrCapacity):
		return "COMPUTER_QUEUE_FULL"
	case errors.Is(err, computercontrol.ErrUnavailable), errors.Is(err, computercontrol.ErrLeaseLost), errors.Is(err, computercontrol.ErrNode):
		return "COMPUTER_UNAVAILABLE"
	case errors.Is(err, ErrUncertain):
		return "EXTERNAL_RESULT_UNKNOWN"
	case errors.Is(err, botagent.ErrModelOutput):
		return "MODEL_OUTPUT_INVALID"
	case errors.Is(err, context.DeadlineExceeded):
		return "RUN_TIMEOUT"
	default:
		return "EXECUTION_FAILED"
	}
}

func (worker *Worker) execute(ctx context.Context, lease Lease) (string, error) {
	work, err := worker.executor.Start(ctx, lease)
	if err != nil {
		return "", err
	}
	selection, err := worker.executor.BindModel(ctx, lease)
	if err != nil {
		return "", err
	}
	allowed := map[string]Tool{}
	var localTools []string
	schemas := make([]provider.ToolSchema, 0, len(work.Bot.ToolPermissions))
	for _, name := range work.Bot.ToolPermissions {
		tool := worker.tools[name]
		if tool == nil {
			return "", ErrUnavailable
		}
		allowed[name] = tool
		if runsInAgent(tool) {
			localTools = append(localTools, name)
		}
		schemas = append(schemas, worker.schemas[name])
	}
	system := work.Bot.SystemPrompt
	if work.HistoryTruncated {
		system += "\nEarlier conversation history was omitted due to context limits. Do not assume access to omitted messages."
	}
	messages := make([]provider.Message, 0, len(work.Messages))
	for _, message := range work.Messages {
		role := "user"
		if message.SenderKind == "bot" {
			role = "assistant"
		}
		if message.SenderKind == "system" {
			role = "system"
		}
		blocks := make([]domain.MessageBlock, 0, len(message.Blocks))
		for _, block := range message.Blocks {
			blocks = append(blocks, domain.MessageBlock{Kind: block.Kind, Text: block.Text})
		}
		messages = append(messages, provider.Message{Role: role, Blocks: blocks})
	}
	previous := map[string]Operation{}
	for _, operation := range work.Operations {
		previous[operation.StepKey] = operation
	}
	start := botagent.Start{LocalTools: localTools, Input: modelgateway.Input{System: system, Messages: messages, Tools: schemas}, Limits: botagent.Limits{ModelSteps: worker.options.MaxModelSteps, ToolCalls: worker.options.MaxToolCalls, OutputBytes: worker.options.MaxOutputBytes, ContextBytes: worker.executor.options.ContextBytes}}
	var session computer.ExecutionSession
	for {
		session, err = worker.computers.Acquire(ctx, computerReference(lease))
		if err == nil {
			break
		}
		if !errors.Is(err, computercontrol.ErrWaiting) {
			return "", err
		}
		if !waitPoll(ctx, worker.options.PollInterval) {
			return "", context.Cause(ctx)
		}
	}
	answer, err := botagent.Dispatch(ctx, worker.agents, session, start, &workerRelay{worker: worker, lease: lease, session: session, selection: selection, approvalMode: work.Bot.ApprovalMode, allowed: allowed, previous: previous}, worker.options.AgentTimeout)
	if errors.Is(err, botagent.ErrProtocol) {
		err = errors.Join(ErrUncertain, err)
	}
	return answer, err
}

// workerRelay 只处理一次操作的授权、持久恢复与结果记账，不决定下一轮模型或工具调用。
// 每次请求复验所属电脑执行权，模型与工具循环只在绑定容器的独立进程内运行。
type workerRelay struct {
	worker       *Worker
	lease        Lease
	session      computer.ExecutionSession
	selection    modelregistry.Selection
	approvalMode string
	allowed      map[string]Tool
	previous     map[string]Operation
	pendingLocal *localOperation
}

func (relay *workerRelay) Model(ctx context.Context, key string, input modelgateway.Input) (botagent.ModelOutput, error) {
	if err := relay.worker.computers.Check(ctx, computerReference(relay.lease)); err != nil {
		return botagent.ModelOutput{}, err
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return botagent.ModelOutput{}, err
	}
	if len(encoded) > relay.worker.executor.options.ContextBytes {
		return botagent.ModelOutput{}, ErrContextLimit
	}
	digest := sha256.Sum256(encoded)
	if operation, exists := relay.previous[key]; exists {
		if operation.Kind != "model" || !bytes.Equal(operation.RequestDigest, digest[:]) {
			return botagent.ModelOutput{}, ErrConflict
		}
		if operation.Status == "failed" {
			return botagent.ModelOutput{}, botagent.ErrModelOutput
		}
		if operation.Status != "succeeded" {
			return botagent.ModelOutput{}, ErrUncertain
		}
		var output botagent.ModelOutput
		if json.Unmarshal(operation.Result, &output) != nil {
			return botagent.ModelOutput{}, botagent.ErrModelOutput
		}
		return output, nil
	}
	for {
		output, err := relay.worker.callModel(ctx, relay.lease, relay.session, relay.selection, key, digest[:], input, relay.allowed)
		// 仅在原子预留明确未成功时等待名额；已发出的请求永不重试。
		if !errors.Is(err, modelusage.ErrConcurrency) {
			return output, err
		}
		if !waitPoll(ctx, relay.worker.options.PollInterval) {
			return botagent.ModelOutput{}, context.Cause(ctx)
		}
	}
}

func (relay *workerRelay) Tool(ctx context.Context, key string, call botagent.ToolCall) (botagent.ToolResult, error) {
	if err := relay.worker.computers.Check(ctx, computerReference(relay.lease)); err != nil {
		return botagent.ToolResult{}, err
	}
	return relay.worker.callTool(ctx, relay.lease, relay.session, relay.approvalMode, key, call, relay.allowed, relay.previous)
}

func (worker *Worker) callModel(ctx context.Context, lease Lease, session computer.ExecutionSession, selection modelregistry.Selection, key string, digest []byte, input modelgateway.Input, allowed map[string]Tool) (botagent.ModelOutput, error) {
	var output botagent.ModelOutput
	err := worker.directory.Use(ctx, selection, func(callContext context.Context, configuration modelregistry.Configuration, credential []byte) error {
		if len(input.Tools) > 0 && !configuration.SupportsTools {
			return ErrUnavailable
		}
		for _, tool := range allowed {
			if tool.RequiresVision() && !configuration.SupportsVision {
				return ErrUnavailable
			}
		}
		resolved, err := worker.executor.ResolveScreenshots(callContext, lease, input)
		if err != nil {
			return err
		}
		input = resolved
		operation, created, err := worker.executor.BeginOperation(callContext, lease, OperationRequest{StepKey: key, Kind: "model", RequestDigest: digest, Computer: &session, Model: &ModelCall{Configuration: configuration, Lifetime: worker.options.ModelTimeout + worker.options.RecordTimeout}})
		if err != nil {
			return err
		}
		if !created {
			if operation.Status == "failed" {
				return botagent.ErrModelOutput
			}
			if operation.Status != "succeeded" {
				return ErrUncertain
			}
			if json.Unmarshal(operation.Result, &output) != nil {
				return botagent.ErrModelOutput
			}
			return nil
		}
		bounded, stop := context.WithTimeout(callContext, worker.options.ModelTimeout)
		collector := newCollector(allowed, worker.options.MaxOutputBytes, worker.options.MaxToolCalls)
		response, callErr := worker.caller.Call(bounded, configuration, credential, input, collector.emit)
		stop()
		var outputErr error
		output, outputErr = collector.result()
		status, code := "succeeded", ""
		if callErr != nil || !response.IsComplete {
			status, code = "unknown", "MODEL_RESULT_UNKNOWN"
		} else if outputErr != nil {
			status, code = "failed", "MODEL_OUTPUT_INVALID"
		}
		encoded, err := json.Marshal(output)
		if err != nil {
			return err
		}
		if len(encoded) > 56<<10 {
			encoded = json.RawMessage(`{"text":"","tools":[],"stop_reason":""}`)
			output = botagent.ModelOutput{}
			if response.IsComplete {
				status, code = "failed", "MODEL_OUTPUT_INVALID"
			}
		}
		writeContext, finish := context.WithTimeout(context.WithoutCancel(ctx), worker.options.RecordTimeout)
		defer finish()
		_, err = worker.executor.RecordOperation(writeContext, lease, operation.ID, OperationResult{Status: status, Result: encoded, ErrorCode: code, Usage: response.Usage, UsageFinal: response.IsComplete})
		if err != nil {
			evidenceContext, stopEvidence := context.WithTimeout(context.WithoutCancel(ctx), worker.options.RecordTimeout)
			defer stopEvidence()
			evidenceErr := worker.executor.AppendModelEvidence(evidenceContext, lease, operation.ID, operation.ID, modelusage.Observation{Usage: response.Usage, Final: response.IsComplete})
			return errors.Join(err, evidenceErr)
		}
		if status == "unknown" {
			return ErrUncertain
		}
		if status == "failed" {
			return botagent.ErrModelOutput
		}
		return nil
	})
	return output, err
}

func (worker *Worker) callTool(ctx context.Context, lease Lease, granted computer.ExecutionSession, approvalMode, key string, call botagent.ToolCall, allowed map[string]Tool, previous map[string]Operation) (botagent.ToolResult, error) {
	tool := allowed[call.Name]
	server, ok := tool.(ServerTool)
	if !ok || runsInAgent(tool) {
		return botagent.ToolResult{}, botagent.ErrModelOutput
	}
	operation, cached, err := worker.prepareTool(ctx, lease, granted, key, call, tool, previous)
	if err != nil || cached != nil {
		if cached != nil {
			return *cached, err
		}
		return botagent.ToolResult{}, err
	}
	bounded, stop := context.WithTimeout(ctx, worker.options.ToolTimeout)
	result, callErr := server.Execute(bounded, Invocation{Lease: lease, OperationID: operation.ID, Sequence: operation.Sequence, Call: call, ApprovalMode: approvalMode, Computer: &granted})
	callErr = errors.Join(callErr, bounded.Err())
	stop()
	code := ""
	if callErr != nil {
		code = "TOOL_RESULT_UNKNOWN"
	}
	return worker.recordTool(ctx, lease, operation, result, code)
}

// prepareTool 在执行前持久登记；已开始或未知的操作不能通过恢复路径再次执行。
func (worker *Worker) prepareTool(ctx context.Context, lease Lease, granted computer.ExecutionSession, key string, call botagent.ToolCall, tool Tool, previous map[string]Operation) (Operation, *botagent.ToolResult, error) {
	encoded, err := json.Marshal(call)
	if err != nil {
		return Operation{}, nil, err
	}
	digest := sha256.Sum256(encoded)
	kind := "tool"
	if tool.UsesComputer() {
		kind = "computer"
	}
	if operation, exists := previous[key]; exists {
		if operation.Kind != kind || !bytes.Equal(operation.RequestDigest, digest[:]) {
			return Operation{}, nil, ErrConflict
		}
		if operation.Status == "started" || operation.Status == "unknown" {
			return Operation{}, nil, ErrUncertain
		}
		var result botagent.ToolResult
		if json.Unmarshal(operation.Result, &result) != nil {
			return Operation{}, nil, ErrInvalidInput
		}
		return operation, &result, nil
	}
	session := &granted
	operation, created, err := worker.executor.BeginOperation(ctx, lease, OperationRequest{StepKey: key, Kind: kind, ToolName: call.Name, RequestDigest: digest[:], Computer: session})
	if err != nil {
		return Operation{}, nil, err
	}
	if !created {
		if operation.Status == "started" || operation.Status == "unknown" {
			return Operation{}, nil, ErrUncertain
		}
		var result botagent.ToolResult
		if json.Unmarshal(operation.Result, &result) != nil {
			return Operation{}, nil, ErrInvalidInput
		}
		return operation, &result, nil
	}
	return operation, nil, nil
}

// recordTool 是本地与服务端工具共用的结果规则；未知结果保留屏障，图片和回执原子保存。
func (worker *Worker) recordTool(ctx context.Context, lease Lease, operation Operation, result botagent.ToolResult, failure string) (botagent.ToolResult, error) {
	status, code := "succeeded", ""
	if failure != "" {
		status, code = "unknown", failure
		result = botagent.ToolResult{Content: "The tool result is unknown. Do not repeat the action.", IsError: true}
	}
	if result.Validate(worker.options.MaxOutputBytes) != nil {
		status, code = "unknown", "TOOL_RESULT_INVALID"
		result = botagent.ToolResult{Content: "The tool returned an invalid or oversized result. Do not repeat the action.", IsError: true}
	}
	if result.IsError && status == "succeeded" {
		status, code = "failed", "TOOL_FAILED"
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return botagent.ToolResult{}, err
	}
	if len(encoded) > 56<<10 {
		status, code = "unknown", "TOOL_RESULT_INVALID"
		result = botagent.ToolResult{Content: "The tool result exceeds the storage limit. Do not repeat the action.", IsError: true}
		encoded, err = json.Marshal(result)
		if err != nil {
			return botagent.ToolResult{}, err
		}
	}
	writeContext, finish := context.WithTimeout(context.WithoutCancel(ctx), worker.options.RecordTimeout)
	defer finish()
	if _, err = worker.executor.RecordOperation(writeContext, lease, operation.ID, OperationResult{Status: status, ErrorCode: code, Result: encoded, Screenshot: result.Artifact}); err != nil {
		if !errors.Is(err, ErrImageCapacity) {
			return botagent.ToolResult{}, err
		}
		// 配额事务已明确回滚；把只读截图的容量拒绝持久记录，避免留下虚假的图片引用。
		result = botagent.ToolResult{Content: "Screenshot storage is full. The image was not saved. Stop requesting screenshots until capacity is available.", IsError: true}
		encoded, err = json.Marshal(result)
		if err != nil {
			return botagent.ToolResult{}, err
		}
		if _, err = worker.executor.RecordOperation(writeContext, lease, operation.ID, OperationResult{Status: "failed", ErrorCode: "SCREENSHOT_CAPACITY", Result: encoded}); err != nil {
			return botagent.ToolResult{}, err
		}
	}
	if result.Artifact != nil {
		ref := *result.Artifact
		ref.Content = nil
		result.Artifact = &ref
	}
	if status == "unknown" {
		return result, ErrUncertain
	}
	return result, nil
}

var _ ModelCaller = (*modelgateway.Gateway)(nil)
var _ ModelDirectory = (*modelregistry.Service)(nil)

func computerReference(lease Lease) computercontrol.RunReference {
	return computercontrol.RunReference{UserID: lease.UserID, JobID: lease.JobID, RunID: lease.RunID, BotID: lease.BotID, OwnerID: lease.OwnerID, FencingToken: lease.FencingToken, Attempt: lease.Attempt}
}
