package adminapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/nodecatalog"
)

func (handler *Handler) nodeDetail(w http.ResponseWriter, r *http.Request) {
	detail, err := handler.nodes.GetNode(r.Context(), current(r).principal, chi.URLParam(r, "nodeID"))
	if err != nil {
		handler.nodeError(w, r, err)
		return
	}
	handler.respond(w, r, 200, detail)
}

func (handler *Handler) updateNode(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name                 string `json:"name"`
		Capacity             int    `json:"capacity"`
		IsDraining           *bool  `json:"is_draining"`
		ConfigurationVersion int64  `json:"configuration_version"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	if request.IsDraining == nil {
		handler.nodeError(w, r, nodecatalog.ErrInvalidInput)
		return
	}
	update := nodecatalog.NodeUpdate{Name: request.Name, Capacity: request.Capacity, IsDraining: *request.IsDraining, ConfigurationVersion: request.ConfigurationVersion}
	actor := current(r)
	detail, err := handler.nodes.UpdateNode(r.Context(), actor.principal, chi.URLParam(r, "nodeID"), update, actor.attempt.RequestID)
	if err != nil {
		handler.nodeError(w, r, err)
		return
	}
	handler.respond(w, r, 200, detail)
}

func (handler *Handler) retryNodeVerification(w http.ResponseWriter, r *http.Request) {
	var request struct {
		FencingToken int64 `json:"fencing_token"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	actor := current(r)
	detail, err := handler.nodes.RetryNodeVerification(r.Context(), actor.principal, chi.URLParam(r, "nodeID"), request.FencingToken, actor.attempt.RequestID)
	if err != nil {
		handler.nodeError(w, r, err)
		return
	}
	handler.respond(w, r, 200, detail)
}
