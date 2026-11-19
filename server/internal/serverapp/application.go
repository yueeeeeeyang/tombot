package serverapp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync/atomic"

	"io.yueyang/tom/internal/accounts"
	"io.yueyang/tom/internal/adminapi"
	"io.yueyang/tom/internal/authapi"
	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/clientrelease"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computercontrol"
	"io.yueyang/tom/internal/computertools"
	"io.yueyang/tom/internal/computerupgrade"
	"io.yueyang/tom/internal/execution"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/nodecatalog"
	"io.yueyang/tom/internal/nodeenrollment"
	"io.yueyang/tom/internal/nodefleet"
	"io.yueyang/tom/internal/noderelease"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/userapi"
	"io.yueyang/tom/internal/userevents"
	adminweb "io.yueyang/tom/web/admin"
)

type nodeRuntime interface {
	computer.NodeRuntime
	computer.Runtime
	computer.SessionRuntime
	computer.TerminalRuntime
	computer.DesktopRuntime
	computer.AgentRuntime
	computer.ConsoleRuntime
	computer.DesktopStreamRuntime
}

type Application struct {
	closeNodes    func() error
	configuration Config
	databases     map[postgres.Role]*postgres.Database
	handler       http.Handler
	tls           *tls.Config
	background    []func(context.Context) error
	logger        *slog.Logger
	state         atomic.Int32
}

const (
	stateNew int32 = iota
	stateRunning
	stateStopped
	stateClosed
	stateStopping
)

type materials struct {
	identity, csrf []byte
	models         map[string][]byte
}

func (keys materials) clear() {
	clear(keys.identity)
	clear(keys.csrf)
	for _, key := range keys.models {
		clear(key)
	}
}

// Open 先迁移受管理的 PostgreSQL 数据库，再组装业务服务；失败时不开放业务入口。
// 不转换旧单用户数据库、不创建示例账户，也不注册宿主执行器。
func Open(ctx context.Context, configuration Config, page *adminweb.Handler, logger *slog.Logger) (application *Application, failure error) {
	if logger == nil || page == nil {
		return nil, errors.New("服务日志或内置管理页面未配置")
	}
	if err := configuration.validate(); err != nil {
		return nil, err
	}
	inbound, err := loadTLS(configuration)
	if err != nil {
		return nil, err
	}
	keys := materials{models: map[string][]byte{}}
	defer keys.clear()
	keys.identity, err = readKey(configuration.IdentityKeyFile)
	if err != nil {
		return nil, err
	}
	keys.csrf, err = readKey(configuration.CSRFKeyFile)
	if err != nil {
		return nil, err
	}
	for name, path := range configuration.ModelKeyFiles {
		key, err := readKey(path)
		if err != nil {
			return nil, err
		}
		keys.models[name] = key
	}
	roles := []postgres.Role{postgres.IdentityRole, postgres.ControlRole, postgres.TenantRole, postgres.DispatchRole}
	connections, err := configuration.Database.connections(roles)
	if err != nil {
		return nil, err
	}
	logger.Info("开始检查并迁移 PostgreSQL 数据库")
	if err := configuration.Database.initialize(ctx, connections); err != nil {
		return nil, err
	}
	logger.Info("PostgreSQL 数据库迁移完成")
	databases := map[postgres.Role]*postgres.Database{}
	defer func() {
		if application == nil {
			failure = errors.Join(failure, closeDatabases(databases))
		}
	}()
	for index, role := range roles {
		database, err := configuration.Database.open(ctx, role, connections[index])
		if err != nil {
			return nil, err
		}
		databases[role] = database
	}
	return assemble(configuration, databases, keys, inbound, nil, page, logger)
}

func assemble(configuration Config, databases map[postgres.Role]*postgres.Database, keys materials, inbound *tls.Config, node nodeRuntime, page *adminweb.Handler, logger *slog.Logger) (*Application, error) {
	if inbound == nil || logger == nil || page == nil {
		return nil, errors.New("多用户服务依赖缺失")
	}
	authOptions := configuration.Identity
	authentication, err := identity.New(databases[postgres.IdentityRole], identity.Options{AccessLifetime: authOptions.AccessLifetime.Value(), SessionLifetime: authOptions.SessionLifetime.Value(), VerificationLifetime: authOptions.VerificationLifetime.Value(), RateWindow: authOptions.RateWindow.Value(), AccountAttempts: authOptions.AccountAttempts, SourceAttempts: authOptions.SourceAttempts, PasswordWorkers: authOptions.PasswordWorkers, DeviceLimit: authOptions.DeviceLimit, EncryptionKey: keys.identity})
	if err != nil {
		return nil, err
	}
	enrollment, err := nodeenrollment.New(databases[postgres.ControlRole], authentication, configuration.Node.MaximumNodes)
	if err != nil {
		return nil, err
	}
	if node == nil {
		node, err = newNodeClient(configuration.Node, enrollment)
		if err != nil {
			return nil, err
		}
	}
	fleet, err := nodeenrollment.NewFleet(enrollment, databases[postgres.ControlRole], node, configuration.Node.observerOptions(), configuration.Node.verificationOptions(), configuration.Node.MaximumNodes, logger)
	if err != nil {
		return nil, err
	}
	enrollmentHTTP, err := nodeenrollment.NewHandler(enrollment, logger)
	if err != nil {
		return nil, err
	}
	gate, err := authapi.New(authentication, authapi.Options{AdminOrigin: configuration.AdminOrigin, TrustedProxies: configuration.TrustedProxies, CSRFKey: keys.csrf, RequestTimeout: configuration.HTTP.RequestTimeout.Value()}, logger)
	if err != nil {
		return nil, err
	}
	gateway, err := newGateway(configuration.Models)
	if err != nil {
		return nil, err
	}
	keyring, err := modelregistry.NewKeyring(configuration.ActiveModelKey, keys.models)
	if err != nil {
		return nil, err
	}
	models, err := modelregistry.New(databases[postgres.ControlRole], databases[postgres.TenantRole], authentication, keyring, gateway, gateway, modelregistry.Options{VerificationTimeout: configuration.Models.VerificationTimeout.Value()})
	if err != nil {
		return nil, err
	}
	users, err := accounts.New(databases[postgres.ControlRole], node, authentication, configuration.Provision.options())
	if err != nil {
		return nil, err
	}
	runs := configuration.Runs
	shell, err := execution.NewComputerShell(node, computertools.ShellOptions{CallTimeout: configuration.Computer.NodeTimeout.Value(), PollInterval: configuration.Computer.ShellPollInterval.Value(), MaximumResultBytes: runs.MaxOutputBytes})
	if err != nil {
		return nil, err
	}
	desktopTools, err := execution.NewComputerDesktop(node, computertools.DesktopOptions{CallTimeout: configuration.Computer.NodeTimeout.Value(), PollInterval: configuration.Computer.ShellPollInterval.Value(), StartTimeout: configuration.Computer.DesktopStartTimeout.Value()})
	if err != nil {
		return nil, err
	}
	tools := append([]execution.Tool{shell, execution.ComputerBrowser{}}, desktopTools...)
	toolNames := make([]string, 0, len(tools))
	for _, tool := range tools {
		toolNames = append(toolNames, tool.Schema().Name)
	}
	catalog, err := bots.New(databases[postgres.TenantRole], bots.Options{MaxBots: configuration.MaxBots, MaxGroups: configuration.MaxGroups, ToolNames: toolNames})
	if err != nil {
		return nil, err
	}
	executor, err := execution.New(databases[postgres.TenantRole], databases[postgres.DispatchRole], models, execution.Options{LeaseLifetime: runs.LeaseLifetime.Value(), RecoveryDelay: runs.RecoveryDelay.Value(), MaxAttempts: runs.MaxAttempts, MaxOperations: runs.MaxOperations, ContextMessages: runs.ContextMessages, ContextBytes: runs.ContextBytes, Images: execution.ImageOptions{MaxUserBytes: runs.Images.MaxUserBytes, MaxUserImages: runs.Images.MaxUserImages, ContextImages: runs.Images.ContextImages, Retention: runs.Images.Retention.Value()}})
	if err != nil {
		return nil, err
	}
	computers := configuration.Computer
	control, err := computercontrol.New(databases[postgres.TenantRole], databases[postgres.DispatchRole], node, computercontrol.Options{LeaseLifetime: computers.LeaseLifetime.Value(), WaitTimeout: computers.WaitTimeout.Value(), HoldTimeout: computers.HoldTimeout.Value(), NodeTimeout: computers.NodeTimeout.Value(), RetryDelay: computers.RetryDelay.Value(), MaxWaiting: computers.MaxWaiting, MaxCleanupAttempts: computers.MaxCleanupAttempts}, logger)
	if err != nil {
		return nil, err
	}
	worker, err := execution.NewWorker(executor, models, gateway, tools, control, node, execution.WorkerOptions{Concurrency: runs.Concurrency, PollInterval: runs.PollInterval.Value(), RunTimeout: runs.RunTimeout.Value(), ModelTimeout: runs.ModelTimeout.Value(), ToolTimeout: runs.ToolTimeout.Value(), RecordTimeout: runs.RecordTimeout.Value(), AgentTimeout: runs.AgentTimeout.Value(), MaxModelSteps: runs.MaxModelSteps, MaxToolCalls: runs.MaxToolCalls, MaxOutputBytes: runs.MaxOutputBytes}, logger)
	if err != nil {
		return nil, err
	}
	bootstrap, err := newNodeBootstrap(configuration, inbound, enrollment, logger)
	if err != nil {
		return nil, err
	}
	nodeAdministration, err := nodecatalog.NewAdministration(databases[postgres.ControlRole], authentication)
	if err != nil {
		return nil, err
	}
	management, err := adminapi.New(users, authentication, nodeManagement{bootstrap, nodeAdministration}, models, gate, logger)
	if err != nil {
		return nil, err
	}
	userEvents, err := userevents.New(databases[postgres.TenantRole])
	if err != nil {
		return nil, err
	}
	eventRetention, err := userevents.NewRetention(databases[postgres.ControlRole], configuration.Events.retentionOptions(), logger)
	if err != nil {
		return nil, err
	}
	console, err := execution.NewConsoleService(executor, node, logger)
	if err != nil {
		return nil, err
	}
	user, err := userapi.New(catalog, executor, console, control, userEvents, gate, logger)
	if err != nil {
		return nil, err
	}
	application := &Application{configuration: configuration, databases: databases, tls: inbound, logger: logger, background: []func(context.Context) error{fleet.Run, users.Run, worker.Serve, eventRetention.Run, console.Run}}
	if upgrader, ok := node.(computer.ImageUpgrader); ok {
		migrations := computerupgrade.New(databases[postgres.ControlRole], upgrader, control, configuration.Node.CallTimeout.Value(), logger)
		application.background = append(application.background, migrations.Run)
	}
	if closer, ok := node.(interface{ CloseIdle() error }); ok {
		application.closeNodes = closer.CloseIdle
	}
	router := http.NewServeMux()
	build, _ := debug.ReadBuildInfo()
	router.HandleFunc("/api/v2/version", versionHandler(versionFromBuild(releaseVersion, build), logger))
	clientReleases := clientrelease.Service{DB: databases[postgres.ControlRole], Directory: configuration.ClientReleaseDir}
	if err := clientReleases.Register(router, gate, authentication, logger); err != nil {
		return nil, err
	}
	updates := nodefleet.Service{DB: databases[postgres.ControlRole], Directory: nodefleet.Directory}
	upgradeAdmin, err := updates.Admin(gate, authentication, logger)
	if err != nil {
		return nil, err
	}
	router.Handle(nodefleet.AdminPath, upgradeAdmin)
	router.Handle(noderelease.AgentPath, updates.Agent(logger))
	router.Handle(nodeenrollment.Path, enrollmentHTTP)
	router.Handle(nodeenrollment.BootstrapPath, bootstrap)
	router.Handle("/api/v2/auth/", gate)
	router.Handle("/api/v2/admin/auth/", gate)
	router.Handle("/api/v2/web/auth/", gate)
	router.Handle("/api/v2/admin/", management)
	router.Handle("/api/v2/", user)
	router.Handle("/admin/", page)
	router.Handle("/admin", page)
	router.Handle("/activate", page)
	router.Handle("/login", gate)
	router.Handle("/authorize", gate)
	router.Handle("/account", gate)
	router.Handle("/account/password", gate)
	router.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if _, err := w.Write([]byte(`{"status":"live"}`)); err != nil {
			logger.Warn("健康状态响应未发送完成")
		}
	})
	router.HandleFunc("GET /health/ready", application.ready)
	router.Handle("/{$}", gate)
	application.handler = router
	return application, nil
}

func newGateway(configuration ModelConfig) (*modelgateway.Gateway, error) {
	var roots *x509.CertPool
	if configuration.RootCAFile != "" {
		encoded, err := readFile(configuration.RootCAFile, 1<<20, false)
		if err != nil {
			return nil, err
		}
		roots, err = x509.SystemCertPool()
		if err != nil {
			return nil, errors.New("读取模型系统信任证书失败")
		}
		if !roots.AppendCertsFromPEM(encoded) {
			return nil, errors.New("模型附加信任证书无效")
		}
	}
	return modelgateway.New(configuration.gatewayOptions(roots))
}

// gatewayOptions 让配置生成与实际入口共用模型网络规则，配置校验不读取信任文件或连接网络。
func (configuration ModelConfig) gatewayOptions(roots *x509.CertPool) modelgateway.Options {
	return modelgateway.Options{DeniedNetworks: configuration.DeniedNetworks, RootCAs: roots, ConnectTimeout: configuration.ConnectTimeout.Value(), TLSHandshakeTimeout: configuration.TLSHandshakeTimeout.Value(), ResponseHeaderTimeout: configuration.ResponseHeaderTimeout.Value(), IdleReadTimeout: configuration.IdleReadTimeout.Value(), WriteTimeout: configuration.WriteTimeout.Value(), RequestTimeout: configuration.RequestTimeout.Value(), MaxRequestBytes: configuration.MaxRequestBytes, MaxResponseBytes: configuration.MaxResponseBytes, MaxConcurrentRequests: configuration.Concurrency}
}

func (application *Application) ready(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	ready := application.state.Load() == stateRunning
	ctx, cancel := context.WithTimeout(r.Context(), application.configuration.Database.ConnectTimeout.Value())
	defer cancel()
	for _, database := range application.databases {
		if ready && database.Ping(ctx) != nil {
			ready = false
		}
	}
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	body := []byte(`{"status":"ready","scope":"api"}`)
	if !ready {
		body = []byte(`{"status":"unavailable","scope":"api"}`)
	}
	if _, err := w.Write(body); err != nil {
		application.logger.WarnContext(ctx, "就绪状态响应未发送完成")
	}
}

func closeDatabases(databases map[postgres.Role]*postgres.Database) error {
	var result error
	for _, role := range []postgres.Role{postgres.DispatchRole, postgres.TenantRole, postgres.ControlRole, postgres.IdentityRole} {
		if database := databases[role]; database != nil {
			result = errors.Join(result, database.Close())
		}
	}
	return result
}

// Close 仅在未启动或全部在途工作已停止后关闭连接；退出超时不能假装资源已回收。
func (application *Application) Close() error {
	if !application.state.CompareAndSwap(stateNew, stateClosed) && !application.state.CompareAndSwap(stateStopped, stateClosed) {
		if application.state.Load() == stateClosed {
			return nil
		}
		return errors.New("服务仍有在途工作，不能关闭数据库")
	}
	var err error
	if application.closeNodes != nil {
		err = application.closeNodes()
	}
	return errors.Join(err, closeDatabases(application.databases))
}
