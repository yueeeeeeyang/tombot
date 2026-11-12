package userapi

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/execution"
)

func (handler *Handler) sendMessage(w http.ResponseWriter, r *http.Request) {
	var request execution.SendRequest
	if !handler.decode(w, r, &request, 64<<10) {
		return
	}
	submission, err := handler.executor.Send(r.Context(), current(r).principal, chi.URLParam(r, "conversationID"), request)
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	status := http.StatusOK
	if submission.IsCreated {
		status = http.StatusAccepted
	}
	w.Header().Set("Location", "/api/v2/runs/"+submission.RunID)
	handler.respond(w, r, status, map[string]any{"submission": submission})
}

func (handler *Handler) listMessages(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		handler.serviceError(w, r, execution.ErrInvalidInput)
		return
	}
	before, limit := int64(0), int64(50)
	for key, values := range query {
		if len(values) != 1 || key != "before" && key != "limit" {
			handler.serviceError(w, r, execution.ErrInvalidInput)
			return
		}
		value, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || value < 1 || strconv.FormatInt(value, 10) != values[0] || key == "limit" && value > 100 {
			handler.serviceError(w, r, execution.ErrInvalidInput)
			return
		}
		if key == "before" {
			before = value
		} else {
			limit = value
		}
	}
	messages, err := handler.executor.Messages(r.Context(), current(r).principal, chi.URLParam(r, "conversationID"), before, int(limit))
	handler.result(w, r, "messages", messages, err)
}

func (handler *Handler) markRead(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Sequence *int64 `json:"sequence"`
	}
	if !handler.decode(w, r, &request, 4096) {
		return
	}
	if request.Sequence == nil {
		handler.serviceError(w, r, execution.ErrInvalidInput)
		return
	}
	if err := handler.executor.MarkRead(r.Context(), current(r).principal, chi.URLParam(r, "conversationID"), *request.Sequence); err != nil {
		handler.serviceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (handler *Handler) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := handler.executor.GetRun(r.Context(), current(r).principal, chi.URLParam(r, "runID"))
	handler.result(w, r, "run", run, err)
}

func (handler *Handler) cancelRun(w http.ResponseWriter, r *http.Request) {
	if !handler.decode(w, r, &struct{}{}, 4096) {
		return
	}
	run, err := handler.executor.Cancel(r.Context(), current(r).principal, chi.URLParam(r, "runID"))
	handler.result(w, r, "run", run, err)
}
