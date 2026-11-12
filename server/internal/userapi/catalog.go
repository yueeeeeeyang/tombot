package userapi

import (
	"encoding/base64"
	"net/http"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/bots"
)

func (handler *Handler) snapshot(w http.ResponseWriter, r *http.Request) {
	_, limit, _, ok := handler.page(w, r, true)
	if !ok {
		return
	}
	value, err := handler.catalog.Snapshot(r.Context(), current(r).principal, limit)
	handler.result(w, r, "snapshot", value, err)
}
func (handler *Handler) updateWorkspace(w http.ResponseWriter, r *http.Request) {
	var request bots.WorkspaceRequest
	if !handler.decode(w, r, &request, 4096) {
		return
	}
	value, err := handler.catalog.UpdateWorkspace(r.Context(), current(r).principal, request)
	handler.result(w, r, "workspace", value, err)
}
func (handler *Handler) listBots(w http.ResponseWriter, r *http.Request) {
	before, limit, archived, ok := handler.page(w, r, false)
	if !ok {
		return
	}
	value, err := handler.catalog.List(r.Context(), current(r).principal, before, limit, archived)
	handler.result(w, r, "bots", value, err)
}
func (handler *Handler) getBot(w http.ResponseWriter, r *http.Request) {
	value, err := handler.catalog.Get(r.Context(), current(r).principal, chi.URLParam(r, "botID"))
	handler.result(w, r, "bot", value, err)
}
func (handler *Handler) createBot(w http.ResponseWriter, r *http.Request) {
	var request bots.CreateRequest
	if !handler.decode(w, r, &request, 64<<10) {
		return
	}
	value, created, err := handler.catalog.Create(r.Context(), current(r).principal, request)
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	status := 200
	if created {
		status = 201
	}
	w.Header().Set("Location", "/api/v2/bots/"+value.ID)
	handler.respond(w, r, status, map[string]any{"bot": value, "is_created": created})
}
func (handler *Handler) updateBot(w http.ResponseWriter, r *http.Request) {
	var request bots.UpdateRequest
	if !handler.decode(w, r, &request, 64<<10) {
		return
	}
	value, err := handler.catalog.Update(r.Context(), current(r).principal, chi.URLParam(r, "botID"), request)
	handler.result(w, r, "bot", value, err)
}
func (handler *Handler) botState(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExpectedRevision int64 `json:"expected_revision"`
		IsArchived       *bool `json:"is_archived"`
	}
	if !handler.decode(w, r, &request, 4096) {
		return
	}
	if request.IsArchived == nil {
		handler.serviceError(w, r, bots.ErrInvalidInput)
		return
	}
	value, err := handler.catalog.SetArchived(r.Context(), current(r).principal, chi.URLParam(r, "botID"), request.ExpectedRevision, *request.IsArchived)
	handler.result(w, r, "bot", value, err)
}
func (handler *Handler) getAvatar(w http.ResponseWriter, r *http.Request) {
	value, err := handler.catalog.GetAvatar(r.Context(), current(r).principal, chi.URLParam(r, "botID"))
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Disposition", `inline; filename="avatar.png"`)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.WriteHeader(200)
	if _, err = w.Write(value.Content); err != nil {
		handler.logger.WarnContext(r.Context(), "写入头像响应失败", "request_id", current(r).attempt.RequestID, "user_id", current(r).principal.UserID)
	}
}
func (handler *Handler) putAvatar(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExpectedRevision int64  `json:"expected_revision"`
		Content          string `json:"content_base64"`
	}
	if !handler.decode(w, r, &request, 100<<10) {
		return
	}
	if len(request.Content) > base64.StdEncoding.EncodedLen(bots.MaxAvatarBytes) {
		handler.serviceError(w, r, bots.ErrInvalidInput)
		return
	}
	content, err := base64.StdEncoding.Strict().DecodeString(request.Content)
	if err != nil {
		handler.serviceError(w, r, bots.ErrInvalidInput)
		return
	}
	value, err := handler.catalog.PutAvatar(r.Context(), current(r).principal, chi.URLParam(r, "botID"), request.ExpectedRevision, content)
	handler.result(w, r, "bot", value, err)
}
func (handler *Handler) removeAvatar(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !handler.decode(w, r, &request, 4096) {
		return
	}
	value, err := handler.catalog.RemoveAvatar(r.Context(), current(r).principal, chi.URLParam(r, "botID"), request.ExpectedRevision)
	handler.result(w, r, "bot", value, err)
}
func (handler *Handler) listGroups(w http.ResponseWriter, r *http.Request) {
	before, limit, archived, ok := handler.page(w, r, false)
	if !ok {
		return
	}
	value, err := handler.catalog.ListGroups(r.Context(), current(r).principal, before, limit, archived)
	handler.result(w, r, "groups", value, err)
}
func (handler *Handler) getGroup(w http.ResponseWriter, r *http.Request) {
	value, err := handler.catalog.GetGroup(r.Context(), current(r).principal, chi.URLParam(r, "groupID"))
	handler.result(w, r, "group", value, err)
}
func (handler *Handler) createGroup(w http.ResponseWriter, r *http.Request) {
	var request bots.CreateGroupRequest
	if !handler.decode(w, r, &request, 16<<10) {
		return
	}
	value, created, err := handler.catalog.CreateGroup(r.Context(), current(r).principal, request)
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	status := 200
	if created {
		status = 201
	}
	w.Header().Set("Location", "/api/v2/groups/"+value.ID)
	handler.respond(w, r, status, map[string]any{"group": value, "is_created": created})
}
func (handler *Handler) updateGroup(w http.ResponseWriter, r *http.Request) {
	var request bots.UpdateGroupRequest
	if !handler.decode(w, r, &request, 16<<10) {
		return
	}
	value, err := handler.catalog.UpdateGroup(r.Context(), current(r).principal, chi.URLParam(r, "groupID"), request)
	handler.result(w, r, "group", value, err)
}
func (handler *Handler) groupState(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExpectedRevision int64 `json:"expected_revision"`
		IsArchived       *bool `json:"is_archived"`
	}
	if !handler.decode(w, r, &request, 4096) {
		return
	}
	if request.IsArchived == nil {
		handler.serviceError(w, r, bots.ErrInvalidInput)
		return
	}
	value, err := handler.catalog.SetGroupArchived(r.Context(), current(r).principal, chi.URLParam(r, "groupID"), request.ExpectedRevision, *request.IsArchived)
	handler.result(w, r, "group", value, err)
}
func (handler *Handler) listConversations(w http.ResponseWriter, r *http.Request) {
	before, limit, archived, ok := handler.page(w, r, false)
	if !ok {
		return
	}
	value, err := handler.catalog.ListConversations(r.Context(), current(r).principal, before, limit, archived)
	handler.result(w, r, "conversations", value, err)
}
func (handler *Handler) getConversation(w http.ResponseWriter, r *http.Request) {
	value, err := handler.catalog.GetConversation(r.Context(), current(r).principal, chi.URLParam(r, "conversationID"))
	handler.result(w, r, "conversation", value, err)
}
