package serverapp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"

	"io.yueyang/tom/internal/accounts"
	"io.yueyang/tom/internal/adminapi"
	"io.yueyang/tom/internal/authapi"
	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computercontrol"
	"io.yueyang/tom/internal/computertools"
	"io.yueyang/tom/internal/execution"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/nodeapi"
	"io.yueyang/tom/internal/nodecatalog"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/userapi"
	adminweb "io.yueyang/tom/web/admin"
)

type nodeRuntime interface {
	computer.NodeRuntime
	computer.Runtime
	computer.SessionRuntime
	computer.TerminalRuntime
	computer.DesktopRuntime
	computer.AgentRuntime
}

type Application struct {
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

// Open 只组装最终 PostgreSQL 服务；不迁移旧库、不创建示例账户，也不注册宿主执行器。
func Open(ctx context.Context, configuration Config, logger *slog.Logger) (application *Application, failure error) {
	if logger == nil {
		return nil, errors.New("服务日志未配置")
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
	node, err := nodeapi.NewClient(nodeapi.ClientOptions{NodeID: configuration.Node.ID, Socket: configuration.Node.Socket, CallTimeout: configuration.Node.CallTimeout.Value()})
	if err != nil {
		return nil, err
	}
	return assemble(configuration, databases, keys, inbound, node, logger)
}

func assemble(configuration Config, databases map[postgres.Role]*postgres.Database, keys materials, inbound *tls.Config, node nodeRuntime, logger *slog.Logger) (*Application, error) {
	if node == nil || inbound == nil || logger == nil {
		return nil, errors.New("多用户服务依赖缺失")
	}
	nodes, err := nodecatalog.New(databases[postgres.ControlRole], configuration.Node.ID)
	if err != nil {
		return nil, err
	}
	observer, err := nodecatalog.NewObserver(nodes, node, configuration.Node.observerOptions(), logger)
	if err != nil {
		return nil, err
	}
	verifier, err := nodecatalog.NewVerifier(nodes, node, configuration.Node.verificationOptions(), logger)
	if err != nil {
		return nil, err
	}
	authOptions := configuration.Identity
	authentication, err := identity.New(databases[postgres.IdentityRole], identity.Options{AccessLifetime: authOptions.AccessLifetime.Value(), SessionLifetime: authOptions.SessionLifetime.Value(), VerificationLifetime: authOptions.VerificationLifetime.Value(), RateWindow: authOptions.RateWindow.Value(), AccountAttempts: authOptions.AccountAttempts, SourceAttempts: authOptions.SourceAttempts, PasswordWorkers: authOptions.PasswordWorkers, DeviceLimit: authOptions.DeviceLimit, EncryptionKey: keys.identity})
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
	models, err := modelregistry.New(databases[postgres.ControlRole], databases[postgres.TenantRole], authentication, keyring, gateway, modelregistry.Options{VerificationTimeout: configuration.Models.VerificationTimeout.Value()})
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
	management, err := adminapi.New(users, models, gate, logger)
	if err != nil {
		return nil, err
	}
	user, err := userapi.New(catalog, executor, gate, logger)
	if err != nil {
		return nil, err
	}
	page, err := adminweb.New(os.DirFS(configuration.AdminDirectory))
	if err != nil {
		return nil, err
	}
	application := &Application{configuration: configuration, databases: databases, tls: inbound, logger: logger, background: []func(context.Context) error{observer.Run, verifier.Run, users.Run, worker.Serve}}
	router := http.NewServeMux()
	router.Handle("/api/v2/auth/", gate)
	router.Handle("/api/v2/admin/auth/", gate)
	router.Handle("/api/v2/admin/", management)
	router.Handle("/api/v2/", user)
	router.Handle("/admin/", page)
	router.Handle("/admin", page)
	router.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if _, err := w.Write([]byte(`{"status":"live"}`)); err != nil {
			logger.Warn("健康状态响应未发送完成")
		}
	})
	router.HandleFunc("GET /health/ready", application.ready)
	router.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusTemporaryRedirect)
	})
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
	origins := make([]modelgateway.Origin, 0, len(configuration.Origins))
	for _, origin := range configuration.Origins {
		origins = append(origins, modelgateway.Origin{URL: origin.URL, Networks: origin.Networks})
	}
	return modelgateway.Options{Origins: origins, DeniedNetworks: configuration.DeniedNetworks, RootCAs: roots, ConnectTimeout: configuration.ConnectTimeout.Value(), TLSHandshakeTimeout: configuration.TLSHandshakeTimeout.Value(), ResponseHeaderTimeout: configuration.ResponseHeaderTimeout.Value(), IdleReadTimeout: configuration.IdleReadTimeout.Value(), WriteTimeout: configuration.WriteTimeout.Value(), RequestTimeout: configuration.RequestTimeout.Value(), MaxRequestBytes: configuration.MaxRequestBytes, MaxResponseBytes: configuration.MaxResponseBytes, MaxConcurrentRequests: configuration.Concurrency}
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
	return closeDatabases(application.databases)
}
