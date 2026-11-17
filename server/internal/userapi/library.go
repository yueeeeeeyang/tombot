package userapi

import (
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/grouplibrary"
	"io.yueyang/tom/internal/httpjson"
)

func (handler *Handler) listGroupFiles(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		handler.serviceError(w, r, grouplibrary.ErrInput)
		return
	}
	for key, values := range query {
		if (key != "before" && key != "q") || len(values) != 1 {
			handler.serviceError(w, r, grouplibrary.ErrInput)
			return
		}
	}
	page, err := handler.executor.GroupFiles(r.Context(), current(r).principal, chi.URLParam(r, "groupID"), query.Get("before"), query.Get("q"))
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	handler.respond(w, r, 200, page)
}
func (handler *Handler) uploadGroupFile(w http.ResponseWriter, r *http.Request) {
	var upload grouplibrary.Upload
	if failure := httpjson.DecodeAttachment(r, &upload, (grouplibrary.MaxFileBytes+2)/3*4+4096); failure != nil {
		handler.failure(w, r, failure.Status, failure.Code, failure.Message)
		return
	}
	file, err := handler.executor.UploadGroupFile(r.Context(), current(r).principal, chi.URLParam(r, "groupID"), upload)
	handler.result(w, r, "file", file, err)
}
func (handler *Handler) groupFileContent(w http.ResponseWriter, r *http.Request) {
	content, err := handler.executor.GroupFileContent(r.Context(), current(r).principal, chi.URLParam(r, "groupID"), chi.URLParam(r, "fileID"))
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	handler.respond(w, r, 200, content)
}
func (handler *Handler) renameGroupFile(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExpectedRevision int64  `json:"expected_revision"`
		Name             string `json:"name"`
	}
	if !handler.decode(w, r, &request, 4096) {
		return
	}
	file, err := handler.executor.RenameGroupFile(r.Context(), current(r).principal, chi.URLParam(r, "groupID"), chi.URLParam(r, "fileID"), request.ExpectedRevision, request.Name)
	handler.result(w, r, "file", file, err)
}
func (handler *Handler) deleteGroupFile(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !handler.decode(w, r, &request, 4096) {
		return
	}
	err := handler.executor.DeleteGroupFile(r.Context(), current(r).principal, chi.URLParam(r, "groupID"), chi.URLParam(r, "fileID"), request.ExpectedRevision)
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
