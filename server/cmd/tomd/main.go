package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"io.yueyang/tom/internal/agent"
	"io.yueyang/tom/internal/auth"
	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/config"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/httpapi"
	"io.yueyang/tom/internal/infra/bus"
	secure "io.yueyang/tom/internal/infra/crypto"
	safelog "io.yueyang/tom/internal/infra/log"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/provision"
	"io.yueyang/tom/internal/secret"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/store"
	"io.yueyang/tom/internal/tools"
	"io.yueyang/tom/internal/workspace"
)

var version = "0.1.0-dev"

func main() {
	logger := slog.New(safelog.NewHandler(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(logger); err != nil {
		logger.Error("服务退出", "原因", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) (result error) {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 {
		if os.Args[1] == "doctor" && len(os.Args) == 2 {
			return doctor(ctx, cfg)
		}
		return archiveCommand(ctx, cfg, os.Args[1:], os.Getenv("TOM_MASTER_KEY"), os.Stdout)
	}
	inboundTLS, err := loadServerTLS(cfg)
	if err != nil {
		return err
	}
	lock, err := store.LockDirectory(cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, lock.Close()) }()
	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, "tom.db"))
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, db.Close()) }()
	events := bus.New(db)
	provisioner, err := provision.New(ctx, db, events, provision.Steps(cfg, db, provision.CommandRunner{}), cfg.StepTimeout)
	if err != nil {
		return err
	}
	key, err := secure.LoadKey(cfg.DataDir, os.Getenv("TOM_MASTER_KEY"))
	if err != nil {
		return err
	}
	defer clear(key)
	authentication, err := auth.New(db, cfg.DataDir)
	if err != nil {
		return err
	}
	code, err := authentication.IssueSetupCode()
	if err != nil {
		return err
	}
	if code != "" {
		if _, err = fmt.Fprintf(os.Stdout, "一次性初始化码（仅在客户端初始化时使用，请勿转发）：%s\n", code); err != nil {
			return err
		}
	}
	if err = os.MkdirAll(cfg.WorkspaceDir, 0700); err != nil {
		return err
	}
	storage, err := workspace.Open(cfg.WorkspaceDir)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, storage.Close()) }()
	catalog := service.NewCatalog(db, service.CatalogOptions{DeepSeekBaseURL: cfg.DeepSeekBaseURL, DeepSeekContextLimit: cfg.DeepSeekContextLimit, DeepSeekOutputLimit: cfg.DeepSeekOutputLimit})
	bots := service.NewBots(db, events, cfg.WorkspaceDir)
	if authentication.Initialized() {
		if err = bots.EnsureDefault(ctx); err != nil {
			return err
		}
	}
	memories := service.NewMemories(db, bots, storage, events)
	memoryContext, stopMemory := context.WithCancel(ctx)
	memoryDone := make(chan struct{})
	go func() { defer close(memoryDone); memories.Maintain(memoryContext, logger) }()
	defer func() { stopMemory(); <-memoryDone }()
	secrets := secret.New(db, key)
	approvals := service.NewApprovals(db, events)
	terminals, err := service.NewTerminals(ctx, db, events, bots, storage, logger)
	if err != nil {
		return err
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		result = errors.Join(result, terminals.Shutdown(shutdown))
	}()
	settings := service.NewSettings(db)
	browsing, err := service.NewBrowser(ctx, service.BrowserDependencies{DB: db, Bots: bots, Events: events, Logger: logger, Options: browser.Options{Executable: provision.BrowserPath(cfg.DataDir), ProfileDir: filepath.Join(cfg.DataDir, "browser-profile")}})
	if err != nil {
		return err
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), browser.OperationTimeout)
		defer cancel()
		result = errors.Join(result, browsing.Shutdown(shutdown))
	}()
	factory := func(spec domain.Provider, credential []byte) provider.Provider {
		return provider.NewHTTP(spec, credential)
	}
	reviewer, err := policy.NewReviewer(policy.ReviewerDependencies{DB: db, Settings: settings, Catalog: catalog, Secrets: secrets, Factory: factory, Logger: logger})
	if err != nil {
		return err
	}
	executor, err := tools.New(db, events, storage, approvals, secrets, terminals, browsing, reviewer, memories)
	if err != nil {
		return err
	}
	limits := agent.DefaultLimits()
	limits.Concurrent, limits.MaxSteps, limits.WallTime, limits.BudgetMicro = cfg.MaxConcurrentRuns, cfg.RunMaxSteps, cfg.RunTimeout, cfg.RunBudgetMicro
	runtime, err := agent.New(ctx, agent.Dependencies{Files: storage, Tools: executor, DB: db, Events: events, Catalog: catalog, Bots: bots, Memories: memories, Secrets: secrets, Logger: logger, Factory: factory}, limits)
	if err != nil {
		return err
	}
	defer runtime.Close()
	routines := service.NewRoutines(db, events, runtime)
	if err = routines.Recover(ctx); err != nil {
		return err
	}
	routineContext, stopRoutines := context.WithCancel(ctx)
	routineDone := make(chan struct{})
	cats, err := service.New(service.Dependencies{DB: db, Key: key, Logger: logger, Bots: bots, Catalog: catalog, Secrets: secrets, WorkspaceDir: cfg.WorkspaceDir})
	if err != nil {
		stopRoutines()
		return err
	}
	api := &httpapi.Server{Audit: service.NewAuditLog(db), Usage: service.NewUsage(db), Memories: memories, Groups: service.NewGroups(db, events), Settings: settings, Files: service.NewFiles(db, bots, storage, events, memories), Runtime: runtime, Approvals: approvals, Catalog: catalog, Bots: bots, Conversations: service.NewConversations(db, events), Secrets: secrets, Config: cfg, Auth: authentication, Workspace: cats, Provision: provisioner, Bus: events, Key: key, Logger: logger, Version: version, Started: time.Now(), RootContext: ctx}
	api.Terminals = terminals
	api.Routines = routines
	api.Browser = browsing
	server := &http.Server{Addr: cfg.Addr, TLSConfig: inboundTLS, Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	failures := make(chan error, 1)
	bootstrapDone := make(chan struct{})
	companion := os.Getenv("TOM_DESKTOP_COMPANION") == "1"
	if !companion {
		if !provisioner.Claim() {
			stopRoutines()
			return fmt.Errorf("引导已在运行")
		}
	}
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		stopRoutines()
		return err
	}
	go func() { failures <- serveListener(server, listener) }()
	go func() {
		defer close(routineDone)
		routines.Maintain(routineContext, logger, provisioner.WaitReady)
	}()
	defer func() { stopRoutines(); <-routineDone }()
	if companion {
		// 桌面阶段不安装浏览器和系统字体。工作空间接口不依赖引导完成，其余业务仍等待引导。
		logger.Info("桌面配套模式，跳过主机引导", "addr", cfg.Addr)
		close(bootstrapDone)
	} else {
		go func() {
			defer close(bootstrapDone)
			if err := provisioner.RunClaimed(ctx, false); err != nil {
				logger.Error("启动引导失败", "phase", provisioner.Snapshot().Phase)
			} else {
				if err := browsing.Start(ctx); err != nil {
					logger.Error("持久浏览器启动失败，浏览器功能暂不可用", "component", "browser")
				}
				logger.Info("服务已就绪", "version", version, "addr", cfg.Addr)
			}
		}()
	}
	logger.Info("服务监听中", "addr", cfg.Addr, "phase", "detecting")
	select {
	case err = <-failures:
		if !errors.Is(err, http.ErrServerClosed) {
			stop()
			<-bootstrapDone
			api.WaitMaintenance()
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = server.Shutdown(shutdown)
	if err != nil {
		err = errors.Join(err, server.Close())
	}
	stop()
	<-bootstrapDone
	provisioner.Wait()
	api.WaitMaintenance()
	return err
}

// doctor 只使用内存数据库做探测，不创建业务数据库、主密钥或认证文件。
func doctor(ctx context.Context, cfg config.Config) (result error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() { result = errors.Join(result, db.Close()) }()
	state, checkErr := provision.Diagnose(ctx, provision.Steps(cfg, db, provision.CommandRunner{}), cfg.StepTimeout)
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintln(os.Stdout, string(encoded)); err != nil {
		return err
	}
	return checkErr
}
