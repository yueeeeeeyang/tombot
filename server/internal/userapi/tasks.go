package userapi

import (
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/scheduledtasks"
)

func (handler *Handler) listTasks(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		handler.serviceError(w, r, scheduledtasks.ErrInput)
		return
	}
	for key, values := range query {
		if key != "before" || len(values) != 1 {
			handler.serviceError(w, r, scheduledtasks.ErrInput)
			return
		}
	}
	page, err := handler.executor.Tasks(r.Context(), current(r).principal, chi.URLParam(r, "botID"), query.Get("before"))
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	handler.respond(w, r, 200, page)
}
func (handler *Handler) taskState(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExpectedRevision int64 `json:"expected_revision"`
		IsEnabled        *bool `json:"is_enabled"`
	}
	if !handler.decode(w, r, &request, 4096) {
		return
	}
	if request.IsEnabled == nil {
		handler.serviceError(w, r, scheduledtasks.ErrInput)
		return
	}
	task, err := handler.executor.SetTaskEnabled(r.Context(), current(r).principal, chi.URLParam(r, "botID"), chi.URLParam(r, "taskID"), request.ExpectedRevision, *request.IsEnabled)
	handler.result(w, r, "task", task, err)
}
func (handler *Handler) deleteTask(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !handler.decode(w, r, &request, 4096) {
		return
	}
	err := handler.executor.DeleteTask(r.Context(), current(r).principal, chi.URLParam(r, "botID"), chi.URLParam(r, "taskID"), request.ExpectedRevision)
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
