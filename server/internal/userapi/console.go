package userapi

import (
	"context"
	"net"
	"net/http"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/desktopstream"
	"io.yueyang/tom/internal/identity"
)

// ConsoleIO 不接受节点、容器或桌面地址，身份必须由持久观看记录派生。
type ConsoleIO interface {
	Ready(context.Context, identity.Principal, string) error
	Stream(context.Context, identity.Principal, string) (net.Conn, error)
}

func (handler *Handler) openConsole(w http.ResponseWriter, r *http.Request) {
	var input struct {
		RequestID string `json:"client_request_id"`
	}
	if !handler.decode(w, r, &input, 1024) {
		return
	}
	result, err := handler.executor.OpenConsole(r.Context(), current(r).principal, chi.URLParam(r, "botID"), input.RequestID)
	handler.result(w, r, "console", result, err)
}
func (handler *Handler) consoleStatus(w http.ResponseWriter, r *http.Request) {
	var input struct{}
	if !handler.decode(w, r, &input, 1024) {
		return
	}
	principal, id := current(r).principal, chi.URLParam(r, "consoleID")
	result, err := handler.executor.ConsoleStatus(r.Context(), principal, id)
	if err == nil && result.State == "opening" {
		err = handler.console.Ready(r.Context(), principal, id)
		if err == nil {
			result, err = handler.executor.ConsoleStatus(r.Context(), principal, id)
		}
	}
	handler.result(w, r, "console", result, err)
}
func (handler *Handler) closeConsole(w http.ResponseWriter, r *http.Request) {
	var input struct{}
	if !handler.decode(w, r, &input, 1024) {
		return
	}
	result, err := handler.executor.CloseConsole(r.Context(), current(r).principal, chi.URLParam(r, "consoleID"))
	handler.result(w, r, "console", result, err)
}
func (handler *Handler) takeConsole(w http.ResponseWriter, r *http.Request) {
	handler.changeConsole(w, r, true)
}
func (handler *Handler) releaseConsole(w http.ResponseWriter, r *http.Request) {
	handler.changeConsole(w, r, false)
}
func (handler *Handler) changeConsole(w http.ResponseWriter, r *http.Request, takeover bool) {
	var input struct{}
	if !handler.decode(w, r, &input, 1024) {
		return
	}
	result, err := handler.executor.ControlConsole(r.Context(), current(r).principal, chi.URLParam(r, "consoleID"), takeover)
	handler.result(w, r, "console", result, err)
}
func (handler *Handler) consoleStream(w http.ResponseWriter, r *http.Request) {
	principal, id := current(r).principal, chi.URLParam(r, "consoleID")
	upstream, err := handler.console.Stream(r.Context(), principal, id)
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		if upstream.Close() != nil {
			handler.logger.Warn("关闭桌面观看入口失败", "user_id", principal.UserID, "console_id", id)
		}
		return
	}
	downstream := websocket.NetConn(r.Context(), connection, websocket.MessageBinary)
	connection.SetReadLimit(1 << 20)
	if err := desktopstream.Pipe(r.Context(), downstream, upstream); err != nil && r.Context().Err() == nil {
		handler.logger.WarnContext(r.Context(), "桌面观看连接中断", "user_id", principal.UserID, "console_id", id)
	}
}
