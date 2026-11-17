package userapi

import (
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/chatlibrary"
	"io.yueyang/tom/internal/httpjson"
)

func (handler *Handler) listChatFiles(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		handler.serviceError(w, r, chatlibrary.ErrInput)
		return
	}
	for key, values := range query {
		if (key != "before" && key != "q") || len(values) != 1 {
			handler.serviceError(w, r, chatlibrary.ErrInput)
			return
		}
	}
	page, err := handler.executor.ChatFiles(r.Context(), current(r).principal, chi.URLParam(r, "conversationID"), query.Get("before"), query.Get("q"))
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	handler.respond(w, r, 200, page)
}
func (handler *Handler) uploadChatFile(w http.ResponseWriter, r *http.Request) {
	var upload chatlibrary.Upload
	if failure := httpjson.DecodeAttachment(r, &upload, (chatlibrary.MaxFileBytes+2)/3*4+4096); failure != nil {
		handler.failure(w, r, failure.Status, failure.Code, failure.Message)
		return
	}
	file, err := handler.executor.UploadChatFile(r.Context(), current(r).principal, chi.URLParam(r, "conversationID"), upload)
	handler.result(w, r, "file", file, err)
}
func (handler *Handler) chatFileContent(w http.ResponseWriter, r *http.Request) {
	content, err := handler.executor.ChatFileContent(r.Context(), current(r).principal, chi.URLParam(r, "conversationID"), chi.URLParam(r, "fileID"))
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	handler.respond(w, r, 200, content)
}
func (handler *Handler) renameChatFile(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExpectedRevision int64  `json:"expected_revision"`
		Name             string `json:"name"`
	}
	if !handler.decode(w, r, &request, 4096) {
		return
	}
	file, err := handler.executor.RenameChatFile(r.Context(), current(r).principal, chi.URLParam(r, "conversationID"), chi.URLParam(r, "fileID"), request.ExpectedRevision, request.Name)
	handler.result(w, r, "file", file, err)
}
func (handler *Handler) deleteChatFile(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !handler.decode(w, r, &request, 4096) {
		return
	}
	err := handler.executor.DeleteChatFile(r.Context(), current(r).principal, chi.URLParam(r, "conversationID"), chi.URLParam(r, "fileID"), request.ExpectedRevision)
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
