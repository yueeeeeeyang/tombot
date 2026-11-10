package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/agent"
	"io.yueyang/tom/internal/auth"
	"io.yueyang/tom/internal/config"
	"io.yueyang/tom/internal/desktop"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/provision"
	"io.yueyang/tom/internal/secret"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/system"
	"io.yueyang/tom/web/health"
)

type contextKey string

const requestKey contextKey = "request_id"
const deviceKey contextKey = "device_id"

// Server 只提供 API、实时通道与纯展示健康页；业务服务通过依赖注入组合。
type Server struct {
	maintenanceMu       sync.Mutex
	maintenanceDone     chan struct{}
	terminalConnections atomic.Int64
	browserConnections  atomic.Int64
	Terminals           *service.Terminals
	Browser             *service.Browser
	Files               *service.Files
	Memories            *service.Memories
	Routines            *service.Routines
	Runtime             *agent.Runtime
	Approvals           *service.Approvals
	Catalog             *service.Catalog
	Settings            *service.Settings
	Audit               *service.AuditLog
	Usage               *service.Usage
	Bots                *service.Bots
	Groups              *service.Groups
	Conversations       *service.Conversations
	Secrets             *secret.Manager
	Metrics             *system.Monitor
	Config              config.Config
	Auth                *auth.Service
	Desktop             *desktop.Service
	Provision           *provision.Provisioner
	Bus                 *bus.Bus
	Key                 []byte
	Logger              *slog.Logger
	Version             string
	Started             time.Time
	RootContext         context.Context
}

// Handler 构建路由并将就绪门禁放在认证与业务处理之前。
func (s *Server) Handler() http.Handler {
	if s.Metrics == nil {
		s.Metrics = system.NewMonitor(s.Config.DataDir)
	}
	router := chi.NewRouter()
	router.Use(s.middleware)
	router.Route("/api/desktop/v1", s.desktopRoutes)
	router.Get("/health", s.healthPage)
	router.Get("/api/v1/health", s.healthJSON)
	router.Route("/api/v1", func(r chi.Router) {
		r.Get("/bootstrap/status", handleRead(s, func(ctx context.Context, _ *http.Request) (any, error) { return s.Provision.Status(ctx) }))
		r.Get("/bootstrap/events", func(w http.ResponseWriter, r *http.Request) { s.events(w, r, true) })
		r.Post("/bootstrap/retry", s.maintainBootstrap(false))
		r.Post("/bootstrap/repair", s.maintainBootstrap(false))
		r.Post("/bootstrap/reset", s.maintainBootstrap(true))
		r.Group(func(r chi.Router) {
			r.Use(s.ready)
			r.Get("/auth/status", s.authStatus)
			r.Post("/auth/setup", s.setup)
			r.Post("/auth/login", s.login)
			r.Group(func(r chi.Router) {
				r.Use(s.authenticate)
				s.configurationRoutes(r)
				s.usageRoutes(r)
				r.Get("/audit", s.listAudit)
				r.Get("/audit/export", s.exportAudit)
				s.groupRoutes(r)
				s.executionRoutes(r)
				s.fileRoutes(r)
				s.memoryRoutes(r)
				s.routineRoutes(r)
				s.terminalRoutes(r)
				s.browserRoutes(r)
				r.Get("/devices", s.devices)
				r.Delete("/devices/{id}", s.revoke)
				r.Post("/devices/push-token", s.pushToken)
				r.Get("/events", func(w http.ResponseWriter, r *http.Request) { s.events(w, r, false) })
				r.Get("/system/status", s.healthJSON)
				r.Get("/system/metrics", s.healthJSON)
			})
		})
	})
	router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		if !s.Provision.Ready() {
			s.fail(w, r, domain.NewError(503, "BOOTSTRAP_NOT_READY", "The server is not ready."))
			return
		}
		s.fail(w, r, domain.NewError(404, "NOT_FOUND", "The resource does not exist."))
	})
	return router
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := ids.New()
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		r = r.WithContext(context.WithValue(r.Context(), requestKey, requestID))
		defer func() {
			if recovered := recover(); recovered != nil {
				if recovered == http.ErrAbortHandler {
					panic(recovered)
				}
				s.Logger.Error("请求处理异常", "request_id", requestID)
				s.fail(w, r, domain.NewError(500, "INTERNAL", "An internal error occurred."))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
func (s *Server) ready(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.Provision.Ready() {
			w.Header().Set("Retry-After", "3")
			s.fail(w, r, domain.NewError(503, "BOOTSTRAP_NOT_READY", "The server is not ready."))
			return
		}
		next.ServeHTTP(w, r)
	})
}
func bearer(r *http.Request) string {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(value, "Bearer ")
}
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := s.Auth.Authenticate(r.Context(), bearer(r))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), deviceKey, id)))
	})
}

func (s *Server) respond(w http.ResponseWriter, r *http.Request, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err = w.Write(append(encoded, '\n')); err != nil {
		s.Logger.Debug("客户端响应已断开", "request_id", r.Context().Value(requestKey))
	}
}
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var failure *domain.Error
	if !errors.As(err, &failure) {
		s.Logger.Error("请求失败", "request_id", r.Context().Value(requestKey))
		failure = domain.NewError(500, "INTERNAL", "An internal error occurred.")
	}
	s.respond(w, r, failure.Status, map[string]any{"error": map[string]any{"code": failure.Code, "kind": failure.Kind, "message": failure.Message, "request_id": r.Context().Value(requestKey)}})
}
func decode(w http.ResponseWriter, r *http.Request, target any) error {
	return decodeLimit(w, r, target, 1<<20)
}
func decodeLimit(w http.ResponseWriter, r *http.Request, target any, maximum int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, maximum)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return domain.NewError(413, "REQUEST_TOO_LARGE", "The request exceeds its size limit.")
		}
		return domain.NewError(400, "INVALID_BODY", "Provide a valid JSON object with supported fields.")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return domain.NewError(400, "INVALID_BODY", "Provide exactly one JSON object.")
	}
	return nil
}

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	authenticated := false
	if bearer(r) != "" {
		_, err := s.Auth.Authenticate(r.Context(), bearer(r))
		authenticated = err == nil
		if err != nil {
			var expected *domain.Error
			if !errors.As(err, &expected) {
				s.fail(w, r, err)
				return
			}
		}
	}
	s.respond(w, r, 200, map[string]any{"initialized": s.Auth.Initialized(), "authenticated": authenticated, "ready": s.Provision.Ready(), "server_version": s.Version})
}
func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Password string `json:"password"`
	}
	if err := decode(w, r, &request); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Auth.Setup(request.Password, r.Header.Get("X-Tom-Setup-Code")); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Bots.EnsureDefault(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Logger.Info("管理员初始化完成", "request_id", r.Context().Value(requestKey))
	s.respond(w, r, 201, map[string]bool{"initialized": true})
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Password string `json:"password"`
		Device   struct {
			Name      string `json:"name"`
			Platform  string `json:"platform"`
			PushToken string `json:"push_token,omitempty"`
		} `json:"device"`
	}
	if err := decode(w, r, &request); err != nil {
		s.fail(w, r, err)
		return
	}
	// 初始化密码已经提交但工作区准备失败时，在签发设备凭据之前重试一次性助手准备。
	if s.Auth.Initialized() {
		if err := s.Bots.EnsureDefault(r.Context()); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	var push *auth.PushRegistration
	if request.Device.PushToken != "" {
		push = &auth.PushRegistration{Token: request.Device.PushToken, Key: s.Key}
	}
	session, err := s.Auth.Login(r.Context(), request.Password, request.Device.Name, request.Device.Platform, push)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Logger.Info("设备登录成功", "device_id", session.DeviceID, "request_id", r.Context().Value(requestKey))
	s.respond(w, r, 200, session)
}
func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 200 {
			s.fail(w, r, domain.NewError(400, "INVALID_LIMIT", "Limit must be between 1 and 200."))
			return
		}
		limit = parsed
	}
	devices, err := s.Auth.List(r.Context(), r.Context().Value(deviceKey).(string), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, 200, devices)
}
func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.Auth.Revoke(r.Context(), id, r.Context().Value(deviceKey).(string)); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Logger.Info("设备撤销请求完成", "device_id", id, "request_id", r.Context().Value(requestKey))
	w.WriteHeader(204)
}
func (s *Server) pushToken(w http.ResponseWriter, r *http.Request) {
	var request struct {
		PushToken string `json:"push_token"`
	}
	if err := decode(w, r, &request); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Auth.UpdatePushToken(r.Context(), r.Context().Value(deviceKey).(string), request.PushToken, s.Key); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(204)
}

// maintainBootstrap 允许初始化码持有者或已授权设备修复环境。认证方式每次请求只选择一次。
func (s *Server) maintainBootstrap(reset bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor := "setup"
		var err error
		if s.Auth.Initialized() {
			actor, err = s.Auth.Authenticate(r.Context(), bearer(r))
			actor = "device:" + actor
		} else {
			err = s.Auth.AuthorizeSetup(r.Header.Get("X-Tom-Setup-Code"))
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if err = s.startMaintenance(r.Context(), actor, reset); err != nil {
			s.fail(w, r, err)
			return
		}
		s.respond(w, r, 202, map[string]string{"status": "started"})
	}
}

// startMaintenance 持有维护名额直到浏览器恢复结束，关闭期间不再接受新任务。
func (s *Server) startMaintenance(ctx context.Context, actor string, reset bool) error {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	if s.RootContext.Err() != nil {
		return domain.NewError(503, "BOOTSTRAP_NOT_READY", "The server is shutting down.")
	}
	if s.maintenanceDone != nil {
		return domain.NewError(409, "BOOTSTRAP_RUNNING", "Bootstrap is already running.")
	}
	if err := s.Provision.ClaimMaintenance(ctx, actor, reset); err != nil {
		return err
	}
	requestID := ctx.Value(requestKey)
	s.maintenanceDone = make(chan struct{})
	go func() {
		defer func() {
			s.maintenanceMu.Lock()
			close(s.maintenanceDone)
			s.maintenanceDone = nil
			s.maintenanceMu.Unlock()
		}()
		if err := s.Provision.RunClaimed(s.RootContext, false); err != nil {
			s.Logger.Error("引导修复失败", "request_id", requestID, "phase", s.Provision.Snapshot().Phase)
		} else if err := s.Browser.Start(s.RootContext); err != nil {
			s.Logger.Error("引导完成但持久浏览器启动失败", "request_id", requestID, "component", "browser")
		}
	}()
	return nil
}

// WaitMaintenance 在 HTTP 停止接收请求后等待维护任务和浏览器恢复，随后才能释放数据库。
func (s *Server) WaitMaintenance() {
	s.maintenanceMu.Lock()
	done := s.maintenanceDone
	s.maintenanceMu.Unlock()
	if done != nil {
		<-done
	}
}

func (s *Server) healthAllowed(w http.ResponseWriter, r *http.Request) bool {
	if s.Config.HealthToken == "" {
		return true
	}
	provided := r.URL.Query().Get("t")
	if _, password, ok := r.BasicAuth(); ok {
		provided = password
	}
	if subtle.ConstantTimeCompare([]byte(provided), []byte(s.Config.HealthToken)) != 1 {
		s.fail(w, r, domain.NewError(401, "AUTH_REQUIRED", "Health access requires authentication."))
		return false
	}
	return true
}

type healthState struct {
	Phase   string                 `json:"phase"`
	PhaseZH string                 `json:"-"`
	Version string                 `json:"version"`
	Uptime  int64                  `json:"uptime_s"`
	Steps   []provision.StepStatus `json:"capabilities"`
	Metrics system.Metrics         `json:"metrics"`
}

func (s *Server) healthSnapshot(ctx context.Context) healthState {
	state := s.Provision.Snapshot()
	steps := append([]provision.StepStatus{}, state.Steps...)
	for i := range steps {
		steps[i].Detail = ""
	}
	phaseZH := map[string]string{"pending": "等待启动", "detecting": "检测环境", "preparing": "准备依赖", "selfcheck": "自检中", "ready": "已就绪", "failed": "引导失败"}[state.Phase]
	return healthState{Phase: state.Phase, PhaseZH: phaseZH, Version: s.Version, Uptime: int64(time.Since(s.Started).Seconds()), Steps: steps, Metrics: s.Metrics.Snapshot(ctx)}
}
func (s *Server) healthJSON(w http.ResponseWriter, r *http.Request) {
	if !s.healthAllowed(w, r) {
		return
	}
	status := http.StatusOK
	if !s.Provision.Ready() {
		status = 503
	}
	s.respond(w, r, status, s.healthSnapshot(r.Context()))
}
func (s *Server) healthPage(w http.ResponseWriter, r *http.Request) {
	if !s.healthAllowed(w, r) {
		return
	}
	page, err := template.New("index.html").Funcs(template.FuncMap{"megabytes": func(value *uint64) any {
		if value == nil {
			return "不可用"
		}
		return *value >> 20
	}, "percent": func(value *float64) string {
		if value == nil {
			return "不可用"
		}
		return fmt.Sprintf("%.1f%%", *value)
	}, "stepStatus": func(value string) string {
		return map[string]string{"ok": "正常", "running": "检查中", "failed": "失败", "degraded": "需关注"}[value]
	}}).ParseFS(health.Files, "index.html")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	if !s.Provision.Ready() {
		w.WriteHeader(503)
	}
	if err = page.Execute(w, s.healthSnapshot(r.Context())); err != nil {
		s.Logger.Debug("健康页面连接已断开", "request_id", r.Context().Value(requestKey))
	}
}

// workspaceEvent 只发布各面板刷新所需的摘要。原始消息、工具输出和引导详情由各自订阅读取。
func workspaceEvent(kind string) bool {
	switch kind {
	case "conversation.updated", "system.notice", "artifact.created", "browser.updated", "browser.availability", "terminal.updated", "memory.updated", "routine.updated":
		return true
	}
	return strings.HasPrefix(kind, "approval.") || strings.HasPrefix(kind, "bot.") || strings.HasPrefix(kind, "group.")
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, bootstrap bool) {
	cursor := int64(0)
	value := r.URL.Query().Get("after")
	if value == "" {
		value = r.Header.Get("Last-Event-ID")
	}
	if value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			s.fail(w, r, domain.NewError(400, "INVALID_CURSOR", "Invalid event cursor."))
			return
		}
		cursor = parsed
	}
	controller := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	write := func(body string) error {
		if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		if _, err := io.WriteString(w, body); err != nil {
			return err
		}
		return controller.Flush()
	}
	if err := write(": connected\n\n"); err != nil {
		return
	}
	conversation := r.URL.Query().Get("conversation_id")
	for {
		wake := s.Bus.Wake()
		events, resync, err := s.Bus.Read(r.Context(), cursor)
		if err != nil {
			s.Logger.Error("事件读取失败", "request_id", r.Context().Value(requestKey))
			return
		}
		if resync {
			if err = write("event: resync\ndata: {}\n\n"); err != nil {
				s.Logger.Debug("事件连接已断开", "request_id", r.Context().Value(requestKey))
			}
			return
		}
		for _, event := range events {
			cursor = event.ID
			if bootstrap && event.Kind != "bootstrap.step" {
				continue
			}
			if !bootstrap {
				if conversation != "" && event.ConversationID != conversation {
					continue
				}
				if conversation == "" && event.Kind == "turn.finished" {
					// 全局只唤醒通知快照，原回合载荷属于会话。
					event.Kind, event.Payload = "notifications.changed", json.RawMessage("{}")
				} else if conversation == "" && !workspaceEvent(event.Kind) {
					continue
				}
			}
			if err = write(fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", event.ID, event.Kind, event.Payload)); err != nil {
				return
			}
		}
		if len(events) == 200 {
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.RootContext.Done():
			return
		case <-wake:
		case <-heartbeat.C:
			if !bootstrap {
				if _, err := s.Auth.Authenticate(r.Context(), bearer(r)); err != nil {
					return
				}
			}
			if err = write(": ping\n\n"); err != nil {
				return
			}
		}
	}
}
